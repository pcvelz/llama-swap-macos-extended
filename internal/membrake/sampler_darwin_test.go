//go:build darwin && cgo

package membrake

import (
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

// The native sampler must read the same counters the observer and vm_stat
// report. This test may exec vm_stat/sysctl; the sampler itself never does.
func TestNativeSamplerMatchesVmStat(t *testing.T) {
	s, err := NewNativeSampler()
	if err != nil {
		t.Fatal(err)
	}
	var r Reading
	if err := s.Sample(&r); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		t.Skip("vm_stat unavailable")
	}
	pg := regexp.MustCompile(`page size of (\d+) bytes`).FindSubmatch(out)
	fb := regexp.MustCompile(`File-backed pages:\s+(\d+)`).FindSubmatch(out)
	wd := regexp.MustCompile(`Pages wired down:\s+(\d+)`).FindSubmatch(out)
	if pg == nil || fb == nil || wd == nil {
		t.Fatalf("could not parse vm_stat:\n%s", out)
	}
	page, _ := strconv.ParseUint(string(pg[1]), 10, 64)
	file, _ := strconv.ParseUint(string(fb[1]), 10, 64)
	wired, _ := strconv.ParseUint(string(wd[1]), 10, 64)
	near := func(name string, got, want uint64) {
		d := int64(got) - int64(want)
		if d < 0 {
			d = -d
		}
		if d > 512<<20 { // the two reads are ms apart on a live machine
			t.Errorf("%s: sampler %d vs vm_stat %d bytes", name, got, want)
		}
	}
	near("file-backed (external_page_count)", r.FileBacked, file*page)
	near("wired", r.Wired, wired*page)

	sw, err := exec.Command("sysctl", "-n", "vm.swapusage").Output()
	if err == nil {
		m := regexp.MustCompile(`used = ([\d.]+)M`).FindSubmatch(sw)
		if m != nil {
			mb, _ := strconv.ParseFloat(string(m[1]), 64)
			near("swap used", r.SwapUsed, uint64(mb*(1<<20)))
		}
	}
	t.Logf("wired %.2f GB, file-backed %.2f GB, swap used %.2f GB", float64(r.Wired)/gib, float64(r.FileBacked)/gib, float64(r.SwapUsed)/gib)
}

func TestNativeSamplerDoesNotAllocate(t *testing.T) {
	s, err := NewNativeSampler()
	if err != nil {
		t.Fatal(err)
	}
	var r Reading
	if a := testing.AllocsPerRun(200, func() { _ = s.Sample(&r) }); a != 0 {
		t.Fatalf("native Sample allocated %.1f times per call", a)
	}
}
