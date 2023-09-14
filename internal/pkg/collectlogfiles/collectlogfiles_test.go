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
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const (
	// Generated by concatenating all strings in logFileNames list in collectlogfiles package
	// with "reproxy_log.txt" (representing the contents of the file at RBE_log_path
	// environment variable) and computing the md5sum of the concatenated string.
	want = "b36efdbca507476203a849f3a76c87a7"

	reproxyLog = "reproxy_log.txt"
)

func TestCreateLogsArchive(t *testing.T) {
	// Setup fake directory and create fake log files inside it.
	tmpDir := t.TempDir()
	glogTestFiles := []string{
		"reproxy.INFO",
		"reproxy.exe.INFO",
		"scandeps.INFO",
		"scandeps.exe.INFO",
		"reproxy.WARNING",
		"reproxy.exe.WARNING",
		"reproxy.ERROR",
		"reproxy.exe.ERROR",
	}

	var logPath string
	for _, fname := range append(logFileNames, glogTestFiles...) {
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

	got := md5sum(t, logFilename)
	if want != got {
		t.Fatalf("Digest mismatch in created log file package, got %v, want %v", got, want)
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
