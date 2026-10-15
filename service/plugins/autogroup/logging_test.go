package autogroup

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2rayA/v2rayA/db/configure"
)

// errTest 是测试里统一的失败原因。
var errTest = errors.New("test failure")

// readEvents 读取日志文件里的所有记录。
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad json line: %v\n%s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

func TestLoggerWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	l.Info(Event{Decision: "pick", Msg: "选中节点", Node: "server/1/0", Candidates: []NodeSnapshot{{Node: "server/1/0", Alive: true, LatencyMs: 88}}})

	evs := readEvents(t, l.Path())
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Decision != "pick" || e.Node != "server/1/0" {
		t.Fatalf("event = %+v", e)
	}
	if e.Level != LevelInfo || e.Seq != 1 || e.TS == "" {
		t.Fatalf("event metadata = %+v", e)
	}
	if len(e.Candidates) != 1 || e.Candidates[0].LatencyMs != 88 {
		t.Fatalf("candidates not persisted: %+v", e.Candidates)
	}
	if e.Caller == "" {
		t.Fatal("caller should be recorded so a log line can be traced back to code")
	}
}

func TestLoggerDisabledWhenDirEmpty(t *testing.T) {
	l := NewLogger("")
	if l.Path() != "" {
		t.Fatal("expected a disabled logger for an empty dir")
	}
	// 写入必须是空操作而不是崩溃。
	l.Info(Event{Decision: "x", Msg: "y"})
	_ = l.Close()
}

// 级别过滤：低于阈值的记录不落盘。
func TestLoggerRespectsLevel(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()
	l.SetLevel(LevelWarn)

	l.Debug(Event{Decision: "cache-hit", Msg: "调试"})
	l.Info(Event{Decision: "pick", Msg: "信息"})
	l.Warn(Event{Decision: "fallback-to-probe", Msg: "警告"})
	l.Error(Event{Decision: "all-unreachable", Msg: "错误"})

	evs := readEvents(t, l.Path())
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (warn+error)", len(evs))
	}
	for _, e := range evs {
		if e.Level == LevelDebug || e.Level == LevelInfo {
			t.Fatalf("filtered level leaked: %+v", e)
		}
	}
}

// SetLevel 要能立即生效，不必重启。
func TestSetLevelTakesEffectImmediately(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()
	l.SetLevel(LevelError)
	l.Info(Event{Decision: "a", Msg: "被过滤"})
	l.SetLevel(LevelDebug)
	l.Info(Event{Decision: "b", Msg: "放行"})

	evs := readEvents(t, l.Path())
	if len(evs) != 1 || evs[0].Decision != "b" {
		t.Fatalf("events = %+v", evs)
	}
}

// 轮转：超过阈值后产生新文件，且旧文件按份数保留。
func TestLoggerRotates(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	// 写超过一个分片的量，强制轮转。
	big := strings.Repeat("x", 4096)
	for i := 0; i < (logMaxBytes/4096)+16; i++ {
		l.Info(Event{Decision: "flood", Msg: big})
	}

	// 主文件应存在且不超过上限。
	st, err := os.Stat(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if st.Size() > logMaxBytes {
		t.Fatalf("current log is %d bytes, over the %d cap", st.Size(), logMaxBytes)
	}
	// 轮转文件应存在。
	if _, err := os.Stat(filepath.Join(dir, logFileName+".1")); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
}

// 序号单调递增，可把一次决策的多条记录串起来。
func TestEventSeqIsMonotonic(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()
	for i := 0; i < 5; i++ {
		l.Info(Event{Decision: "probe"})
	}
	evs := readEvents(t, l.Path())
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("seq not increasing: %d then %d", evs[i-1].Seq, evs[i].Seq)
		}
	}
}

// 快照要能支撑离线复算：给出健康缓存与候选集，能还原出可用节点集合。
func TestSnapshotCandidatesIsReproducible(t *testing.T) {
	now := int64(1_700_000_000)
	refs := []configure.NodeRef{
		{TYPE: configure.ServerType, ID: 1},
		{TYPE: configure.ServerType, ID: 2},
		{TYPE: configure.ServerType, ID: 3},
	}
	health := map[string]configure.NodeHealth{
		refs[0].Key(): {Alive: true, LatencyMs: 120, CheckedAt: now - 5},   // 新鲜
		refs[1].Key(): {Alive: true, LatencyMs: 80, CheckedAt: now - 700}, // 过期
		refs[2].Key(): {Alive: false, Fails: 3, LastError: "TIMEOUT"},
	}
	snaps := SnapshotCandidates(refs, health, now)
	if len(snaps) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(snaps))
	}
	if !snaps[0].Fresh || !snaps[0].Alive || snaps[0].AgeSec != 5 {
		t.Fatalf("snapshot[0] = %+v", snaps[0])
	}
	if snaps[1].Fresh {
		t.Fatal("snapshot[1] should be stale: checked 700s ago")
	}
	if snaps[1].AgeSec != 700 {
		t.Fatalf("snapshot[1].AgeSec = %d, want 700", snaps[1].AgeSec)
	}
	if snaps[2].Alive || snaps[2].Fails != 3 || snaps[2].Err != "TIMEOUT" {
		t.Fatalf("snapshot[2] = %+v", snaps[2])
	}
	if got := CountFresh(snaps); got != 1 {
		t.Fatalf("CountFresh = %d, want 1", got)
	}
}

// 每次决策都要带候选快照——这是"事后能复算"的前提。
func TestEnsureGroupLogsCandidateSnapshot(t *testing.T) {
	dir := t.TempDir()
	nodes := newTestNodes(3)
	ap, _ := setupAutoPilot(t, allAlive(40))
	ap.SetLogger(NewLogger(dir))
	if _, err := ap.EnsureGroup("test", nodes); err != nil {
		t.Fatal(err)
	}
	logPath := ap.log.Path()
	if err := ap.log.Close(); err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, logPath)
	var pick *Event
	for i := range evs {
		if evs[i].Decision == "pick" {
			pick = &evs[i]
		}
	}
	if pick == nil {
		t.Fatalf("no pick event in %d events: %+v", len(evs), evs)
	}
	if len(pick.Candidates) != 3 {
		t.Fatalf("pick event lacks full candidate set: %d entries", len(pick.Candidates))
	}
	if pick.Node == "" || pick.Reason == "" {
		t.Fatalf("pick event missing decision inputs: %+v", pick)
	}
}

// 切换事件必须同时记录 from/to/reason/候选快照。
func TestReportFailureLogsSwitchWithBothNodes(t *testing.T) {
	dir := t.TempDir()
	nodes := newTestNodes(3)
	probeCalls := 0
	ap, _ := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		if probeCalls == 0 {
			probeCalls++
			return aliveMap(nodes, []int{0}, 30), nil
		}
		return aliveMap(nodes, []int{1}, 30), nil
	})
	ap.SetLogger(NewLogger(dir))

	current, err := ap.EnsureGroup("test", nodes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := ap.ReportFailure("test", nodes, current, errTest); err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	logPath := ap.log.Path()
	if err := ap.log.Close(); err != nil {
		t.Fatal(err)
	}

	evs := readEvents(t, logPath)
	var sw *Event
	for i := range evs {
		if evs[i].Decision == "switch" {
			sw = &evs[i]
		}
	}
	if sw == nil {
		t.Fatalf("no switch event: %+v", evs)
	}
	if sw.From == "" || sw.To == "" || sw.From == sw.To {
		t.Fatalf("switch event from/to = %q -> %q", sw.From, sw.To)
	}
	if sw.Fails != 5 || sw.FailThreshold != 5 {
		t.Fatalf("switch event fails/threshold = %d/%d, want 5/5", sw.Fails, sw.FailThreshold)
	}
	if len(sw.Candidates) != 3 {
		t.Fatalf("switch event lacks candidate snapshot")
	}
}

// 未达阈值的失败也要记录，否则看不出"为什么当时没切"。
func TestBelowThresholdIsLogged(t *testing.T) {
	dir := t.TempDir()
	nodes := newTestNodes(2)
	ap, _ := setupAutoPilot(t, allAlive(30))
	ap.SetLogger(NewLogger(dir))

	current, _ := ap.EnsureGroup("test", nodes)
	for i := 0; i < 3; i++ {
		ap.ReportFailure("test", nodes, current, errTest)
	}
	logPath := ap.log.Path()
	_ = ap.log.Close()

	found := 0
	for _, e := range readEvents(t, logPath) {
		if e.Decision == "below-threshold" {
			found++
			if e.Fails == 0 {
				t.Fatal("below-threshold event should carry the fail count")
			}
		}
	}
	if found != 3 {
		t.Fatalf("below-threshold events = %d, want 3", found)
	}
}

// pinned 全灭要记成 error 级，且带完整候选快照。
func TestPinnedAllDeadLoggedAsError(t *testing.T) {
	dir := t.TempDir()
	nodes := newTestNodes(2)
	ap, _ := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		return aliveMap(nodes, nil, 0), nil
	})
	ap.SetLogger(NewLogger(dir))
	policy := configure.GetGroupPolicy("test")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("test", policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGroupPolicy("test") })
	if _, err := ap.EnsureGroup("test", nodes); err == nil {
		t.Fatal("expected error")
	}
	logPath := ap.log.Path()
	_ = ap.log.Close()

	var ae *Event
	for i, e := range readEvents(t, logPath) {
		if e.Decision == "all-unreachable" {
			ae = &readEvents(t, logPath)[i]
		}
	}
	if ae == nil {
		t.Fatal("no all-unreachable event")
	}
	if ae.Level != LevelError {
		t.Fatalf("level = %s, want error", ae.Level)
	}
	if len(ae.Candidates) != 2 {
		t.Fatalf("all-unreachable event lacks candidates")
	}
}

// ParseLogLevel 的兜底行为。
func TestParseLogLevel(t *testing.T) {
	if got := ParseLogLevel("DEBUG", ""); got != LevelDebug {
		t.Fatalf("got %q, want debug", got)
	}
	if got := ParseLogLevel("  Info ", ""); got != LevelInfo {
		t.Fatalf("got %q, want info", got)
	}
	if got := ParseLogLevel("bogus", LevelWarn); got != LevelWarn {
		t.Fatalf("got %q, want the fallback warn", got)
	}
	if got := ParseLogLevel("bogus", ""); got != "" {
		t.Fatalf("got %q, want empty for validation callers", got)
	}
}

// 主日志摘要钩子可被测试替换，不污染真实日志。
func TestMainLogHookReceivesSummary(t *testing.T) {
	var got []string
	SetMainLogHook(func(s string) { got = append(got, "i:"+s) },
		func(s string) { got = append(got, "w:"+s) },
		func(s string) { got = append(got, "e:"+s) })
	defer SetMainLogHook(func(string) {}, func(string) {}, func(string) {})

	l := NewLogger(t.TempDir())
	defer l.Close()
	l.Warn(Event{Decision: "fallback-to-probe", Msg: "改用探测"})
	if len(got) != 1 || !strings.HasPrefix(got[0], "w:[fallback-to-probe]") {
		t.Fatalf("hook got %v", got)
	}
}