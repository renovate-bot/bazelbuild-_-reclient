// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collectlogfiles

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	spb "github.com/bazelbuild/reclient/api/stats"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/fakes"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/filemetadata"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

const (
	// Generated by concatenating all strings in logFileNames list in collectlogfiles package
	// with "reproxy_log.txt" (representing the contents of the file at RBE_log_path
	// environment variable) and computing the md5sum of the concatenated string.
	wantSum = "ac27acdab21f9abeed44e8612d63226f"

	reproxyLog = "reproxy_log.txt"
)

func TestCreateLogsArchive(t *testing.T) {
	// Setup fake directory and create fake log files inside it.
	tmpDir := t.TempDir()
	testFiles := []string{
		"reproxy.INFO",
		"reproxy.hostname.username.log.INFO.20230921-153342.97022",
		"reproxy.exe.INFO",
		"scandeps_server.INFO",
		"scandeps_server.exe.INFO",
		"reproxy.WARNING",
		"reproxy.exe.WARNING",
		"reproxy.ERROR",
		"reproxy.exe.ERROR",
		"reproxy_log.txt",
		"reproxy_2023-09-21_15_33_47.rrpl",
		"reproxy_2023-09-21_15_33_47.rpl",
	}

	var logPath string
	for _, fname := range testFiles {
		p := filepath.Join(tmpDir, fname)
		if err := os.WriteFile(p, []byte(fname), 0644); err != nil {
			t.Fatalf("Unable to write %s to file %v: %v", fname, p, err)
		}
		if fname != reproxyLog {
			continue
		}
		logPath = "text://" + p
	}

	logFile, err := os.CreateTemp("", "reclient-log-collection-test-*.tar.gz")
	if err != nil {
		t.Fatalf("Failed to create temp log file: %v", err)
	}
	logFile.Close()
	logFilename := logFile.Name()
	defer os.RemoveAll(logFilename)
	if err := CreateLogsArchive(logFilename, []string{tmpDir}, logPath); err != nil {
		t.Errorf("CreateLogsArchive(%v) failed: %v", logFilename, err)
	}

	gotFiles := fnamesFromArchive(t, logFilename)
	wantFiles := testFiles

	sort.Strings(gotFiles)
	sort.Strings(wantFiles)

	if diff := cmp.Diff(wantFiles, gotFiles); diff != "" {
		t.Errorf("Got wrong log files in tar.gz\n (-want +got): %v", diff)
	}

	gotSum := md5sum(t, logFilename)
	if wantSum != gotSum {
		t.Fatalf("Digest mismatch in created log file package, got %v, want %v", gotSum, wantSum)
	}
}

func fnamesFromArchive(t *testing.T, logFile string) (files []string) {
	f, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("Unable to open logfile %v: %v", logFile, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("Unable to read created logfile %v: %v", logFile, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		th, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Errorf("Error when reading tar.gz file: %v", err)
			return
		}
		files = append(files, filepath.Base(th.Name))
	}
}

// md5sum computes the md5sum of the contents of the files in given .tar.gz logfile.
func md5sum(t *testing.T, logFile string) string {
	t.Helper()

	// The created gzip file is non-deterministic, so gunzip / untar it back and then
	// compute the digest.
	f, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("Unable to open logfile %v: %v", logFile, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("Unable to read created logfile %v: %v", logFile, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	h := md5.New()
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("Error when reading tar.gz file: %v", err)
			break
		}
		if _, err := io.Copy(h, tr); err != nil {
			t.Errorf("Unable to read %v in .tar.gz file: %v", th.Name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestUploadDirsToCasLogPathInDir(t *testing.T) {
	if runtime.GOOS == "darwin" {
		macTMP := os.Getenv("TMPDIR")
		os.Setenv("TMPDIR", filepath.Join("/private", macTMP))
		t.Cleanup(func() {
			os.Setenv("TMPDIR", macTMP)
		})
	}
	logDir1 := t.TempDir()
	logDir2 := t.TempDir()

	testFiles := []string{
		"reproxy.INFO",
		"reproxy.hostname.username.log.INFO.20230921-153342.97022",
		"reproxy.exe.INFO",
		"scandeps_server.INFO",
		"scandeps_server.exe.INFO",
		"reproxy.WARNING",
		"reproxy.exe.WARNING",
		"reproxy.ERROR",
		"reproxy.exe.ERROR",
		"reproxy_log.txt",
		"reproxy_2023-09-21_15_33_47.rrpl",
		"reproxy_2023-09-21_15_33_47.rpl",
	}

	wantFiles1 := map[string]string{}
	wantFiles2 := map[string]string{}

	var logPath string
	for _, fname := range testFiles {
		if err := os.WriteFile(filepath.Join(logDir1, fname), []byte(fname+"1"), 0644); err != nil {
			t.Fatalf("Unable to write %s to file %v: %v", fname+"1", filepath.Join(logDir1, fname), err)
		}
		wantFiles1[fname] = fname + "1"
		if err := os.WriteFile(filepath.Join(logDir2, fname), []byte(fname+"2"), 0644); err != nil {
			t.Fatalf("Unable to write %s to file %v: %v", fname+"2", filepath.Join(logDir2, fname), err)
		}
		if fname != reproxyLog {
			wantFiles2[fname] = fname + "2"
			continue
		}
		logPath = "text://" + filepath.Join(logDir1, fname)
	}

	env, cleanup := fakes.NewTestEnv(t)
	t.Cleanup(cleanup)
	fmc := filemetadata.NewSingleFlightCache()
	env.Client.FileMetadataCache = fmc
	got, err := UploadDirsToCas(env.Client.GrpcClient, []string{logDir1, logDir2}, logPath)
	if err != nil {
		t.Errorf("UploadDirsToCas returned unexpected error: %v", err)
	}
	want := []*spb.LogDirectory{{
		Path:   logDir1,
		Digest: "937edd392257d4358cfa51560c9b06fa25ca6414f7afb8b2445975b05f700a61/1159",
	}, {
		Path:   logDir2,
		Digest: "5503db5384fb7f098c44596583a876615613ad8b1cfacf811e123c82aa9b46e7/1070",
	}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
	logDir1Dg, _ := digest.NewFromString("937edd392257d4358cfa51560c9b06fa25ca6414f7afb8b2445975b05f700a61/1159")
	if diff := cmp.Diff(wantFiles1, getFileContentsFromCasDir(t, env.Client.GrpcClient, logDir1Dg), protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
	logDir2Dg, _ := digest.NewFromString("5503db5384fb7f098c44596583a876615613ad8b1cfacf811e123c82aa9b46e7/1070")
	if diff := cmp.Diff(wantFiles2, getFileContentsFromCasDir(t, env.Client.GrpcClient, logDir2Dg), protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
}

func TestUploadDirsToCasLogPathInOtherDir(t *testing.T) {
	if runtime.GOOS == "darwin" {
		macTMP := os.Getenv("TMPDIR")
		os.Setenv("TMPDIR", filepath.Join("/private", macTMP))
		t.Cleanup(func() {
			os.Setenv("TMPDIR", macTMP)
		})
	}
	logDir1 := t.TempDir()
	logDir2 := t.TempDir()
	logPathDir := t.TempDir()

	testFiles := []string{
		"reproxy.INFO",
		"reproxy.hostname.username.log.INFO.20230921-153342.97022",
		"reproxy.exe.INFO",
		"scandeps_server.INFO",
		"scandeps_server.exe.INFO",
		"reproxy.WARNING",
		"reproxy.exe.WARNING",
		"reproxy.ERROR",
		"reproxy.exe.ERROR",
		"reproxy_2023-09-21_15_33_47.rrpl",
		"reproxy_2023-09-21_15_33_47.rpl",
	}

	wantFiles1 := map[string]string{}
	wantFiles2 := map[string]string{}

	var logPath string
	for _, fname := range testFiles {
		if err := os.WriteFile(filepath.Join(logDir1, fname), []byte(fname+"1"), 0644); err != nil {
			t.Fatalf("Unable to write %s to file %v: %v", fname+"1", filepath.Join(logDir1, fname), err)
		}
		wantFiles1[fname] = fname + "1"
		if err := os.WriteFile(filepath.Join(logDir2, fname), []byte(fname+"2"), 0644); err != nil {
			t.Fatalf("Unable to write %s to file %v: %v", fname+"2", filepath.Join(logDir2, fname), err)
		}
		wantFiles2[fname] = fname + "2"
	}
	if err := os.WriteFile(filepath.Join(logPathDir, "reproxy_log.txt"), []byte("logpathcontent"), 0644); err != nil {
		t.Fatalf("Unable to write %s to file %v: %v", "logpathcontent", filepath.Join(logPathDir, "reproxy_log.txt"), err)
	}
	logPath = "text://" + filepath.Join(logPathDir, "reproxy_log.txt")

	env, cleanup := fakes.NewTestEnv(t)
	t.Cleanup(cleanup)
	fmc := filemetadata.NewSingleFlightCache()
	env.Client.FileMetadataCache = fmc
	got, err := UploadDirsToCas(env.Client.GrpcClient, []string{logDir1, logDir2}, logPath)
	if err != nil {
		t.Errorf("UploadDirsToCas returned unexpected error: %v", err)
	}
	want := []*spb.LogDirectory{{
		Path:   logDir1,
		Digest: "e699ee321eec2acfb354acf7a364aa42abf4438515c1c93323bb8e791dfb9e40/1070",
	}, {
		Path:   logDir2,
		Digest: "5503db5384fb7f098c44596583a876615613ad8b1cfacf811e123c82aa9b46e7/1070",
	}, {
		Path:   logPathDir,
		Digest: "d67dbaf338940e0ff3fa5f7fd155d925b2ed518dd9d1ce6618ee927bcf52cd80/89",
	}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
	logDir1Dg, _ := digest.NewFromString("e699ee321eec2acfb354acf7a364aa42abf4438515c1c93323bb8e791dfb9e40/1070")
	if diff := cmp.Diff(wantFiles1, getFileContentsFromCasDir(t, env.Client.GrpcClient, logDir1Dg), protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
	logDir2Dg, _ := digest.NewFromString("5503db5384fb7f098c44596583a876615613ad8b1cfacf811e123c82aa9b46e7/1070")
	if diff := cmp.Diff(wantFiles2, getFileContentsFromCasDir(t, env.Client.GrpcClient, logDir2Dg), protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
	wantLogPathDirFiles := map[string]string{"reproxy_log.txt": "logpathcontent"}
	logPathDirDg, _ := digest.NewFromString("d67dbaf338940e0ff3fa5f7fd155d925b2ed518dd9d1ce6618ee927bcf52cd80/89")
	if diff := cmp.Diff(wantLogPathDirFiles, getFileContentsFromCasDir(t, env.Client.GrpcClient, logPathDirDg), protocmp.Transform()); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
}

func getFileContentsFromCasDir(t *testing.T, grpcClient *client.Client, dg digest.Digest) map[string]string {
	t.Helper()
	st := filemetadata.NewSingleFlightCache()
	outDir := t.TempDir()
	_, _, err := grpcClient.DownloadDirectory(context.Background(), dg, outDir, st)
	if err != nil {
		t.Errorf("Error downloading digest %v: %v", dg.String(), err)
		return nil
	}
	out := map[string]string{}
	filepath.WalkDir(outDir, func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[info.Name()] = string(blob)
		return nil
	})
	return out
}

func TestDeduplicateDirs(t *testing.T) {
	logDir1 := t.TempDir()
	logDir2 := t.TempDir()

	got, err := DeduplicateDirs([]string{logDir1, logDir2, logDir1, logDir2})
	if err != nil {
		t.Errorf("FilterValidLogDirs returned unexpected error: %v", err)
	}

	absLogDir1, err := toAbsRealPath(logDir1)
	if err != nil {
		t.Errorf("Unable to resolve %v: %v", logDir1, err)
	}
	absLogDir2, err := toAbsRealPath(logDir2)
	if err != nil {
		t.Errorf("Unable to resolve %v: %v", logDir2, err)
	}

	if diff := cmp.Diff([]string{absLogDir1, absLogDir2}, got); diff != "" {
		t.Errorf("UploadDirsToCas returned incorrect digests, (-want +got): %v", diff)
	}
}
