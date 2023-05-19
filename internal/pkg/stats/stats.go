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

// Package stats contains functionality that is used to parse the log file
// produced by reproxy, to extract stat information.
package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"team/foundry-x/re-client/internal/pkg/labels"
	"team/foundry-x/re-client/internal/pkg/localresources"
	"team/foundry-x/re-client/internal/pkg/logger"
	"team/foundry-x/re-client/internal/pkg/protoencoding"
	"team/foundry-x/re-client/internal/pkg/reproxystatus"
	"team/foundry-x/re-client/pkg/version"

	"cloud.google.com/go/bigquery"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	lpb "team/foundry-x/re-client/api/log"
	spb "team/foundry-x/re-client/api/stats"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	log "github.com/golang/glog"
)

const (
	// AggregatedMetricsFileBaseName is the base name of the rbe metric file.
	//
	// We typically work with two rbe_metrics files:
	//   rbe_metrics.pb
	//   rbe_metrics.txt
	AggregatedMetricsFileBaseName = "rbe_metrics"
	bwUnit                        = 1000
	bwUnitReps                    = "KMGT"
)

// Stat is a collection of aggregated metrics for a single field.
type Stat struct {
	// The number of all the true values for bools, the sum of all the values for ints.
	Count int64

	// For enum stats, the count of each value.
	CountByValue map[string]int64

	// These fields are relevant to int stats and time intervals:
	rawValues []int64
	isMillis  bool

	// Commands that have the highest values.
	Outlier1, Outlier2 *spb.Outlier

	Median, Percentile75, Percentile85, Percentile95 int64
	Average                                          float64
}

// IsEmpty returns whether the Stat has any non-0 values.
func (st *Stat) IsEmpty() bool {
	return st.Outlier1 == nil && st.Count == 0 && len(st.CountByValue) == 0
}

// Stats is a collection of Stat by field name.
type Stats struct {
	NumRecords                         int64
	invIDs                             map[string]bool
	Stats                              map[string]*Stat
	tree                               *statTree
	mismatches                         []*lpb.Verification_Mismatch
	ProxyInfos                         []*lpb.ProxyInfo
	numVerified                        int64
	cacheHits                          int64
	minProxyExecStart, maxProxyExecEnd float64
}

// New creates a new empty Stats object.
func New() *Stats {
	s := &Stats{
		invIDs:            make(map[string]bool),
		Stats:             make(map[string]*Stat),
		tree:              newStatTree(),
		minProxyExecStart: math.MaxFloat64,
	}
	return s
}

// statTree is a tree struct storing Stat by field name.
type statTree struct {
	name      string
	subfields map[string]*statTree
	value     *Stat
}

func newStatTree() *statTree {
	s := &statTree{
		subfields: make(map[string]*statTree),
		value:     &Stat{},
	}
	return s
}

func (s *Stats) aggregate(stt *statTree, prefix string) {
	if prefix != "" && stt.name != "" {
		prefix += "."
	}
	prefix += stt.name
	st := stt.value
	if len(stt.subfields) == 0 { // It is a leaf node with a valid value.
		if st.isMillis {
			prefix += "Millis"
		}
		s.Stats[prefix] = st
	}
	for _, sttChild := range stt.subfields {
		s.aggregate(sttChild, prefix)
	}
}

func (stt *statTree) child(name string) *statTree {
	sttChild, ok := stt.subfields[name]
	if !ok {
		sttChild = newStatTree()
		sttChild.name = name
		stt.subfields[name] = sttChild
	}
	return sttChild
}

func (stt *statTree) addLogRecord(rec *lpb.LogRecord, name string, cmdID string) {
	recStt := stt.child(name)
	if rec == nil {
		return
	}
	recStt.addCommandResult(rec.Result, "Result", cmdID)
	recStt.addRemoteMetadata(rec.RemoteMetadata, "RemoteMetadata", cmdID)
	recStt.addLocalMetadata(rec.LocalMetadata, "LocalMetadata", cmdID)
	recStt.addStatus(rec.CompletionStatus, "CompletionStatus", cmdID)
}

func (stt *statTree) addCommandResult(res *cpb.CommandResult, name string, cmdID string) {
	resStt := stt.child(name)
	if res == nil {
		return
	}
	resStt.addStatus(&res.Status, "Status", cmdID)
}

func (stt *statTree) addRemoteMetadata(rm *lpb.RemoteMetadata, name string, cmdID string) {
	rmStt := stt.child(name)
	if rm == nil {
		return
	}
	rmStt.addCommandResult(rm.Result, "Result", cmdID)
	rmStt.addBool(rm.CacheHit, "CacheHit", cmdID)
	rmStt.addNum(int64(rm.NumInputFiles), "NumInputFiles", cmdID, false)
	rmStt.addNum(int64(rm.NumInputDirectories), "NumInputDirectories", cmdID, false)
	rmStt.addNum(rm.TotalInputBytes, "TotalInputBytes", cmdID, false)
	rmStt.addNum(int64(rm.NumOutputFiles), "NumOutputFiles", cmdID, false)
	rmStt.addNum(int64(rm.NumOutputDirectories), "NumOutputDirectories", cmdID, false)
	rmStt.addNum(rm.TotalOutputBytes, "TotalOutputBytes", cmdID, false)
	rmStt.addEventTimes(rm.EventTimes, "EventTimes", cmdID) //
	rmStt.addNum(rm.LogicalBytesUploaded, "LogicalBytesUploaded", cmdID, false)
	rmStt.addNum(rm.RealBytesUploaded, "RealBytesUploaded", cmdID, false)
	rmStt.addNum(rm.LogicalBytesDownloaded, "LogicalBytesDownloaded", cmdID, false)
	rmStt.addNum(rm.RealBytesDownloaded, "RealBytesDownloaded", cmdID, false)
	rmStt.addRerunMetadatas(rm.RerunMetadata, "RerunMetadata", cmdID)
}

func (stt *statTree) addLocalMetadata(lm *lpb.LocalMetadata, name string, cmdID string) {
	lmStt := stt.child(name)
	if lm == nil {
		return
	}
	lmStt.addCommandResult(lm.Result, "Result", cmdID)
	lmStt.addBool(lm.ExecutedLocally, "ExecutedLocally", cmdID)
	lmStt.addBool(lm.ValidCacheHit, "ValidCacheHit", cmdID)
	lmStt.addBool(lm.UpdatedCache, "UpdatedCache", cmdID)
	lmStt.addVerification(lm.Verification, "Verification", cmdID)
	lmStt.addEventTimes(lm.EventTimes, "EventTimes", cmdID)
	lmStt.addRerunMetadatas(lm.RerunMetadata, "RerunMetadata", cmdID)
}

func (stt *statTree) addRerunMetadatas(rms []*lpb.RerunMetadata, name string, cmdID string) {
	rmsStt := stt.child(name)
	if rms == nil {
		return
	}
	for _, rm := range rms {
		rmsStt.addRerunMetadata(rm, "RerunMetadata", cmdID)
	}
}

func (stt *statTree) addRerunMetadata(rm *lpb.RerunMetadata, name string, cmdID string) {
	rmStt := stt.child(name)
	if rm == nil {
		return
	}
	rmStt.addNum(rm.Attempt, "Attempt", cmdID, false)
	rmStt.addCommandResult(rm.Result, "Result", cmdID)
	rmStt.addNum(int64(rm.NumOutputFiles), "NumOutputFiles", cmdID, false)
	rmStt.addNum(int64(rm.NumOutputFiles), "NumOutputFiles", cmdID, false)
	rmStt.addNum(int64(rm.NumOutputDirectories), "NumOutputDirectories", cmdID, false)
	rmStt.addNum(rm.TotalOutputBytes, "TotalOutputBytes", cmdID, false)
	rmStt.addNum(rm.LogicalBytesDownloaded, "LogicalBytesDownloaded", cmdID, false)
	rmStt.addNum(rm.RealBytesDownloaded, "RealBytesDownloaded", cmdID, false)
	rmStt.addEventTimes(rm.EventTimes, "EventTimes", cmdID)
}

func (stt *statTree) addVerification(vf *lpb.Verification, name string, cmdID string) {
	vfStt := stt.child(name)
	if vf == nil {
		return
	}
	vfStt.addMismatches(vf.Mismatches, "Mismatches", cmdID)
	vfStt.addNum(int64(vf.TotalMismatches), "TotalMismatches", cmdID, false)
	vfStt.addNum(int64(vf.TotalIgnoredMismatches), "TotalIgnoredMismatches", cmdID, false)
	vfStt.addNum(vf.TotalVerified, "TotalVerified", cmdID, false)
}

func (stt *statTree) addMismatches(mismatches []*lpb.Verification_Mismatch, name string, cmdID string) {
	mmStt := stt.child(name)
	if mismatches == nil {
		return
	}
	for _, mismatch := range mismatches {
		mmStt.addMismatch(mismatch, "Mismatch", cmdID)
	}
}

func (stt *statTree) addMismatch(mm *lpb.Verification_Mismatch, name string, cmdID string) {
	mmStt := stt.child(name)
	if mm == nil {
		return
	}
	mmStt.addBool(mm.NonDeterministic, "NonDeterministic", cmdID)
	mmStt.addBool(mm.Ignored, "Ignored", cmdID)
}

func (stt *statTree) addEventTimes(et map[string]*cpb.TimeInterval, name string, cmdID string) {
	etStt := stt.child(name)
	if et == nil {
		return
	}
	for k, v := range et {
		etStt.addTimeInterval(v, k, cmdID)
	}
}

func (stt *statTree) addTimeInterval(tPb *cpb.TimeInterval, name, cmdID string) {
	tiStt := stt.child(name)
	if tPb == nil {
		return
	}
	ti := command.TimeIntervalFromProto(tPb)
	if !ti.From.IsZero() && !ti.To.IsZero() {
		val := int32(ti.To.Sub(ti.From).Milliseconds())
		tiStt.addNum(int64(val), "", cmdID, true)
	}
}

func (stt *statTree) addNum(val int64, name, cmdID string, isMillis bool) {
	valStt := stt.child(name)
	// Aggregate Stat object for value.
	st := valStt.value
	st.rawValues = append(st.rawValues, val)
	if isMillis {
		st.isMillis = true
		st.Count++
	} else {
		st.Count += val // It makes no sense to add time intervals.
	}
	if val == 0 {
		return
	}
	cur := &spb.Outlier{CommandId: cmdID, Value: int64(val)}
	if st.Outlier1 == nil || val > st.Outlier1.Value {
		st.Outlier2 = st.Outlier1
		st.Outlier1 = cur
		return
	}
	if st.Outlier2 == nil || val > st.Outlier2.Value {
		st.Outlier2 = cur
	}
}

func (stt *statTree) addBool(b bool, name, cmdID string) {
	bStt := stt.child(name)
	st := bStt.value
	if b {
		st.Count++
	}
}

func (stt *statTree) addStatus(res fmt.Stringer, name string, cmdID string) {
	resStt := stt.child(name)
	st := resStt.value
	if st.CountByValue == nil {
		st.CountByValue = make(map[string]int64)
	}
	if res != nil {
		st.CountByValue[res.String()]++
	}
}

// ToProto returns the proto representation of the Stats.
func (s *Stats) ToProto() *spb.Stats {
	sPb := &spb.Stats{
		NumRecords: s.NumRecords,
		ProxyInfo:  []*lpb.ProxyInfo{},
	}
	if s.NumRecords != 0 {
		sPb.BuildCacheHitRatio = float64(s.cacheHits) / float64(s.NumRecords)
		sPb.BuildLatency = s.maxProxyExecEnd - s.minProxyExecStart
	}
	for id := range s.invIDs {
		sPb.InvocationIds = append(sPb.InvocationIds, id)
	}
	var keys []string
	for n := range s.Stats {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		st := s.Stats[n]
		if !st.IsEmpty() {
			sPb.Stats = append(sPb.Stats, statToProto(n, st))
		}
	}
	if s.mismatches != nil {
		sPb.Verification = &lpb.Verification{
			Mismatches:      s.mismatches,
			TotalMismatches: int32(len(s.mismatches)),
			TotalVerified:   s.numVerified,
		}
	}
	sPb.MachineInfo = machineInfo()
	sPb.ProxyInfo = s.ProxyInfos
	return sPb
}

// ProtoSaver is an implentation of bigquery.ValueSaver for spb.Stats
type ProtoSaver struct {
	*spb.Stats
}

// Save implements the bigquery.ValueSaver interface.
func (vs *ProtoSaver) Save() (map[string]bigquery.Value, string, error) {
	if vs == nil || vs.Stats == nil {
		return nil, "", nil
	}
	bs, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(vs.Stats)
	if err != nil {
		return nil, "", err
	}
	var out map[string]bigquery.Value
	err = json.Unmarshal(bs, &out)
	if err != nil {
		return nil, "", err
	}
	// protojson marshals proto3 maps as map[string]Value but bigquery expects them to be []{"key": string, "value": Value}
	if pisRaw, ok := out["proxy_info"]; ok {
		pis := pisRaw.([]any)
		for _, piRaw := range pis {
			pi := piRaw.(map[string]any)
			if pi["event_times"] != nil {
				pi["event_times"] = mapToArr(pi["event_times"])
			}
			if pi["metrics"] != nil {
				pi["metrics"] = mapToArr(pi["metrics"])
			}
			if pi["flags"] != nil {
				pi["flags"] = mapToArr(pi["flags"])
			}
		}
	}
	return out, uuid.New().String(), nil
}

func mapToArr(mRaw any) any {
	m, ok := mRaw.(map[string]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{
			"key":   k,
			"value": v,
		})
	}
	return out
}

func humanReadableBytes(numBytes int64) string {
	if numBytes < bwUnit {
		return fmt.Sprintf("%d B", numBytes)
	}
	res, idx := int64(bwUnit), 0
	for n := numBytes / bwUnit; n >= bwUnit; n /= bwUnit {
		res *= bwUnit
		idx++
	}
	return fmt.Sprintf("%0.2f %cB", float64(numBytes)/float64(res), bwUnitReps[idx])
}

// BandwidthStats returns the human readable form of download and uplaod
// bandwidth consumed by reproxy.
func BandwidthStats(s *spb.Stats) (string, string) {
	var up, down int64
	for _, st := range s.Stats {
		if st.Name == "RemoteMetadata.RealBytesDownloaded" {
			down = int64(st.Count)
		}
		if st.Name == "RemoteMetadata.RealBytesUploaded" {
			up = int64(st.Count)
		}
	}
	return humanReadableBytes(down), humanReadableBytes(up)
}

// CompletionStats returns the human readable form of the number of actions
// executed by reproxy grouped by their completion status.
func CompletionStats(s *spb.Stats) string {
	for _, st := range s.Stats {
		if st.Name == "CompletionStatus" {
			m := make(map[string]int32, len(lpb.CompletionStatus_value))
			for _, valcnt := range st.CountsByValue {
				m[valcnt.Name] = int32(valcnt.Count)
			}
			return reproxystatus.CompletedActionsSummary(m)
		}
	}
	return ""
}

func statToProto(name string, s *Stat) *spb.Stat {
	sPb := &spb.Stat{
		Name:         name,
		Count:        s.Count,
		Median:       s.Median,
		Percentile75: s.Percentile75,
		Percentile85: s.Percentile85,
		Percentile95: s.Percentile95,
		Average:      s.Average,
	}
	var keys []string
	for n := range s.CountByValue {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		v := s.CountByValue[n]
		sPb.CountsByValue = append(sPb.CountsByValue, &spb.Stat_Value{Name: n, Count: v})
	}
	if s.Outlier1 != nil {
		sPb.Outliers = append(sPb.Outliers, s.Outlier1)
	}
	if s.Outlier2 != nil {
		sPb.Outliers = append(sPb.Outliers, s.Outlier2)
	}
	return sPb
}

// AggregateLogToFiles aggregates stats from the given proxy log, adds tool
// version and environment variables, and dumps the result to files in both
// ASCII and binary formats.
func AggregateLogToFiles(formatfile, outputdir string) error {
	var sPb *spb.Stats
	s, err := NewFromLogFile(formatfile)
	if err != nil {
		log.Errorf("Failed reading proxy log: %v", err)
		// If failed reading the log file, still produce output that includes
		// the environment variables and other things.
		sPb = &spb.Stats{}
	} else {
		sPb = s.ToProto()
	}
	return WriteStats(sPb, outputdir)
}

// AggregateLogDirsToFiles aggregates stats from proxy logs in given directories, adds tool
// version and environment variables, and dumps the result to files in both
// ASCII and binary formats.
func AggregateLogDirsToFiles(formatStr string, logDirs []string, outputdir string) error {
	var sPb *spb.Stats
	s, err := NewFromLogDirs(formatStr, logDirs)
	if err != nil {
		log.Errorf("Failed reading proxy log: %v", err)
		// If failed reading the log file, still produce output that includes
		// the environment variables and other things.
		sPb = &spb.Stats{}
	} else {
		sPb = s.ToProto()
	}
	return WriteStats(sPb, outputdir)
}

// WriteStats writes stats to a file.
func WriteStats(sPb *spb.Stats, outputdir string) error {
	if err := os.MkdirAll(outputdir, os.FileMode(0777)); err != nil {
		return err
	}
	path := filepath.Join(outputdir, AggregatedMetricsFileBaseName)
	f, err := os.Create(path + ".txt")
	if err != nil {
		return err
	}
	defer f.Close()
	sPb.ToolVersion = version.CurrentVersion()
	f.WriteString(protoencoding.TextWithIndent.Format(sPb))
	blob, err := proto.Marshal(sPb)
	if err != nil {
		return err
	}
	fb, err := os.Create(path + ".pb")
	if err != nil {
		return err
	}
	defer fb.Close()
	fb.Write(blob)
	return nil
}

// NewFromLogFile creates a new Stats from a proxy Record log file.
func NewFromLogFile(formatfile string) (*Stats, error) {
	recs, err := logger.ParseFromFormatFile(formatfile)
	if err != nil {
		return nil, err
	}
	return NewFromRecords(recs, nil), nil
}

// NewFromLogDirs creates a new Stats from a proxy Record log files under given directories.
func NewFromLogDirs(formatStr string, logDirs []string) (*Stats, error) {
	format, err := logger.ParseFormat(formatStr)
	if err != nil {
		return nil, err
	}
	recs, pInfo, err := logger.ParseFromLogDirs(format, logDirs)
	if err != nil {
		return nil, err
	}
	return NewFromRecords(recs, pInfo), nil
}

// NewFromRecords creates a new Stats from the given Records.
func NewFromRecords(recs []*lpb.LogRecord, pInfos []*lpb.ProxyInfo) *Stats {
	s := New()
	for _, r := range recs {
		s.AddRecord(r)
	}
	s.FinalizeAggregate(pInfos)
	return s
}

// FinalizeAggregate aggregates and finalizes all Stats.
func (s *Stats) FinalizeAggregate(pInfos []*lpb.ProxyInfo) {
	s.ProxyInfos = append(s.ProxyInfos, pInfos...)
	s.aggregate(s.tree, "")
	s.finalize()
}

// AddRecord adds the log record to the statTree.
// It is not thread safe.
func (s *Stats) AddRecord(r *lpb.LogRecord) {
	s.NumRecords++
	s.tree.addLogRecord(r, "", r.Command.GetIdentifiers().GetCommandId())
	l := r.GetLocalMetadata().GetLabels()
	if len(l) != 0 {
		s.tree.addLogRecord(r, labels.ToKey(l), r.Command.GetIdentifiers().GetCommandId())
	}
	s.mismatches = append(s.mismatches, r.LocalMetadata.GetVerification().GetMismatches()...)
	s.numVerified += r.LocalMetadata.GetVerification().GetTotalVerified()
	invID := r.Command.GetIdentifiers().GetInvocationId()
	if invID != "" {
		s.invIDs[invID] = true
	}
	st := r.GetResult().GetStatus()
	if st == cpb.CommandResultStatus_CACHE_HIT {
		s.cacheHits++
	}
	times := r.GetLocalMetadata().GetEventTimes()
	if tPb, ok := times[logger.EventProxyExecution]; ok {
		ti := command.TimeIntervalFromProto(tPb)
		if !ti.From.IsZero() && !ti.To.IsZero() {
			s.minProxyExecStart = math.Min(s.minProxyExecStart, float64(ti.From.Unix()))
			s.maxProxyExecEnd = math.Max(s.maxProxyExecEnd, float64(ti.To.Unix()))
		}
	}
}

func (s *Stats) finalize() {
	for _, st := range s.Stats {
		vals := st.rawValues
		n := len(vals)
		if n > 0 {
			sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
			st.Median = vals[n/2]
			st.Percentile75 = vals[n*3/4]
			st.Percentile85 = vals[n*17/20]
			st.Percentile95 = vals[n*19/20]
			var total float64
			for _, v := range vals {
				total += float64(v)
			}
			st.Average = total / float64(n)
		}
	}
	sort.Slice(s.mismatches, func(a, b int) bool { return s.mismatches[a].Path < s.mismatches[b].Path })
}

func machineInfo() *spb.MachineInfo {
	return &spb.MachineInfo{
		NumCpu:   int64(runtime.NumCPU()),
		RamMbs:   localresources.TotalRAMMBs(),
		OsFamily: runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
}