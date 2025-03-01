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

// Package cppdependencyscanner encapsulates a concrete include scanner.
// It can either encapsulate clang-scan-deps or goma's input processor
// depending on build configuration.
// If specified as an argument it will alternatively connect to a remote scanner service.
package cppdependencyscanner

import (
	"context"
	"errors"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/bazelbuild/reclient/internal/pkg/cppdependencyscanner/depsscannerclient"
	"github.com/bazelbuild/reclient/internal/pkg/features"
	"github.com/bazelbuild/reclient/internal/pkg/ipc"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/outerr"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/retry"

	spb "github.com/bazelbuild/reclient/api/scandeps"
)

// DepsScanner is an include scanner for c++ compiles.
type DepsScanner interface {
	//ProcessInputs receives a compile command, source file, and working directory and returns
	//a list of inputs, a boolean indicating whether deps cache was used, and an error if
	//exists.
	ProcessInputs(ctx context.Context, execID string, compileCommand []string, filename, directory string, cmdEnv []string) ([]string, bool, error)
	Close()
	Capabilities() *spb.CapabilitiesResponse
}

// Executor can run commands and retrieve their outputs.
type executor interface {
	ExecuteInBackground(ctx context.Context, cmd *command.Command, oe outerr.OutErr, ch chan *command.Result) error
}

var (
	// ErrDepsScanTimeout is the error returned by the input processor
	// when it times out during the dependency scanning phase.
	ErrDepsScanTimeout = errors.New("cpp dependency scanner timed out")
	backoff            = retry.ExponentialBackoff(1*time.Second, 10*time.Second, retry.Attempts(10))
	shouldRetry        = func(err error) bool {
		if err == context.DeadlineExceeded {
			return true
		}
		s, ok := status.FromError(err)
		if !ok {
			return false
		}
		switch s.Code() {
		case codes.Canceled, codes.Unknown, codes.DeadlineExceeded, codes.Unavailable, codes.ResourceExhausted:
			return true
		default:
			return false
		}
	}
)

func connect(ctx context.Context, address string) (spb.CPPDepsScannerClient, *spb.CapabilitiesResponse, error) {
	var client spb.CPPDepsScannerClient
	var capabilities *spb.CapabilitiesResponse
	conn, err := ipc.DialContext(ctx, address)
	if err != nil {
		return nil, nil, err
	}
	err = retry.WithPolicy(ctx, shouldRetry, backoff, func() error {
		client = spb.NewCPPDepsScannerClient(conn)
		capabilities, err = client.Capabilities(ctx, &emptypb.Empty{})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return client, capabilities, nil
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.DeadlineExceeded
	}
	return false
}

// New creates new DepsScanner.
func New(ctx context.Context, executor executor, cacheDir, logDir string, cacheSizeMaxMb int, useDepsCache bool, depsScannerAddress, proxyServerAddress string) (DepsScanner, error) {
	ds, err := depsscannerclient.New(ctx, executor, cacheDir, cacheSizeMaxMb, useDepsCache, logDir, depsScannerAddress, proxyServerAddress, features.GetConfig().DepsScannerConnectTimeout, connect)
	if err != nil {
		log.Infof("Timed out or failed connecting to dependency scanner service, restarting service once.  Error was %v", err)
		// Try one more time if we timed out connecting to the service.
		return depsscannerclient.New(ctx, executor, cacheDir, cacheSizeMaxMb, useDepsCache, logDir, depsScannerAddress, proxyServerAddress, features.GetConfig().DepsScannerConnectTimeout, connect)
	}
	return ds, err
}
