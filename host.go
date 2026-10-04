package pulse

import (
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// DiskUsage is the capacity of one mount point, in bytes.
type DiskUsage struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

func (d DiskUsage) Percent() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Used) / float64(d.Total) * 100
}

// HostSample is a point-in-time reading of the machine and the Go runtime.
type HostSample struct {
	Instance   string      `json:"instance"`
	Time       time.Time   `json:"time"`
	PID        int         `json:"pid"`
	GoVersion  string      `json:"go_version"`
	StartedAt  time.Time   `json:"started_at"`
	CPUs       int         `json:"cpus"`
	CPUPercent float64     `json:"cpu_percent"`
	Load1      float64     `json:"load1"`
	Load5      float64     `json:"load5"`
	Load15     float64     `json:"load15"`
	MemTotal   uint64      `json:"mem_total"`
	MemUsed    uint64      `json:"mem_used"`
	Disks      []DiskUsage `json:"disks"`
	Goroutines int         `json:"goroutines"`
	HeapAlloc  uint64      `json:"heap_alloc"`
	NumGC      uint32      `json:"num_gc"`
	Dropped    int64       `json:"dropped"`
}

func (s HostSample) MemPercent() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return float64(s.MemUsed) / float64(s.MemTotal) * 100
}

func (p *Pulse) sampleHost() {
	defer p.wg.Done()
	// The first cpu.Percent call only primes the counter it diffs against.
	_, _ = cpu.Percent(0, false)
	p.emit(hostEvent{p.readHost()})

	ticker := time.NewTicker(p.cfg.HostInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.emit(hostEvent{p.readHost()})
		case <-p.done:
			return
		}
	}
}

func (p *Pulse) readHost() HostSample {
	s := HostSample{
		Instance:   p.cfg.Instance,
		Time:       time.Now(),
		PID:        os.Getpid(),
		GoVersion:  runtime.Version(),
		StartedAt:  p.started,
		CPUs:       runtime.NumCPU(),
		Goroutines: runtime.NumGoroutine(),
		Dropped:    p.dropped.Load(),
	}
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		s.CPUPercent = pct[0]
	}
	if avg, err := load.Avg(); err == nil {
		s.Load1, s.Load5, s.Load15 = avg.Load1, avg.Load5, avg.Load15
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemTotal, s.MemUsed = vm.Total, vm.Used
	}
	for _, path := range p.cfg.DiskPaths {
		if u, err := disk.Usage(path); err == nil {
			s.Disks = append(s.Disks, DiskUsage{Path: path, Total: u.Total, Used: u.Used})
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.HeapAlloc, s.NumGC = ms.HeapAlloc, ms.NumGC
	return s
}
