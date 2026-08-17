package v2ray

import (
	"reflect"
	"testing"
)

func TestProcessCloseMarksExpectedStopBeforeCancel(t *testing.T) {
	process := &Process{template: &Template{}}
	process.procCancel = func() {
		if !process.expectedStop.Load() {
			t.Fatal("expected stop must be recorded before canceling the core")
		}
	}

	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !process.expectedStop.Load() {
		t.Fatal("Close() did not record the expected stop")
	}
}

func TestCoreProcessArgumentsUseStandaloneConfig(t *testing.T) {
	got := coreProcessArguments("v2raya_core.exe", "latency.json", "")
	want := []string{"v2raya_core.exe", "run", "--config=latency.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coreProcessArguments() = %v, want %v", got, want)
	}
}

func TestCoreProcessArgumentsKeepMainConfigDir(t *testing.T) {
	got := coreProcessArguments("v2raya_core.exe", "config.json", "conf.d")
	want := []string{"v2raya_core.exe", "run", "--config=config.json", "--confdir=conf.d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coreProcessArguments() = %v, want %v", got, want)
	}
}
