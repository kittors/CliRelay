// Package nodemetrics samples what one CliRelay process and the machine it
// runs on are doing: host CPU split into running and stolen time, load,
// memory, the process's cgroup quota, usage and throttling, in-flight
// requests, request and byte counts, the egress breaker, and the round trips
// to the database and Redis. Cluster nodes publish the latest snapshot with
// their membership heartbeat so the console can show every node; a single
// node reports its own.
//
// Host CPU comes from /proc/stat rather than from the process or its cgroup.
// On a kernel built without CONFIG_PARAVIRT_TIME_ACCOUNTING (hk-155 in
// 2026-11) the time the hypervisor takes from a vCPU is charged to whatever
// ran on it, so process and cgroup CPU include it, while /proc/stat counts it
// apart as steal. On an oversold host a slot can then look busy while it
// mostly waited for the hypervisor: on 2026-11-16 the host ran 0.76 cores and
// lost 2.76 to steal while the slot's cgroup recorded 2.55.
//
// Everything is read best effort: on a non-Linux system, or when a file is
// missing or unreadable, the affected fields are left out and sampling goes
// on. Nothing here returns an error to the caller.
package nodemetrics

import "time"

// SchemaVersion is the snapshot layout. It changes when a field changes
// meaning or goes away; new fields alone keep it.
const SchemaVersion = 1

// Snapshot is one sample of the process and its machine.
type Snapshot struct {
	Schema      int       `json:"schema"`
	CollectedAt time.Time `json:"collected_at"`
	Version     string    `json:"version,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	// GOMAXPROCS is how many threads may run Go code at once; the runtime
	// derives it from the cgroup CPU quota.
	GOMAXPROCS int `json:"gomaxprocs"`
	Goroutines int `json:"goroutines"`
	// MemoryLimitBytes is the Go runtime's soft memory limit (GOMEMLIMIT),
	// left out when there is none.
	MemoryLimitBytes *int64 `json:"memory_limit_bytes,omitempty"`

	Host    *Host      `json:"host,omitempty"`
	Cgroup  *Cgroup    `json:"cgroup,omitempty"`
	Traffic Traffic    `json:"traffic"`
	Egress  *Egress    `json:"egress,omitempty"`
	DB      *RoundTrip `json:"db,omitempty"`
	Redis   *RoundTrip `json:"redis,omitempty"`
}

// Host describes the whole machine.
type Host struct {
	// CPUs counts the online CPUs listed in /proc/stat.
	CPUs   int      `json:"cpus,omitempty"`
	CPU    *HostCPU `json:"cpu,omitempty"`
	Load   *Load    `json:"load,omitempty"`
	Memory *Memory  `json:"memory,omitempty"`
}

// HostCPU splits the machine's CPU time over the window between the last two
// samples into cores: 1.5 means one and a half CPUs spent the whole window in
// that state. It is left out until there are two samples.
type HostCPU struct {
	WindowSeconds float64 `json:"window_seconds"`
	// BusyCores is time spent running: user, nice, system, irq and softirq.
	BusyCores float64 `json:"busy_cores"`
	// StealCores is time the vCPUs had work but the hypervisor ran something
	// else.
	StealCores  float64 `json:"steal_cores"`
	IOWaitCores float64 `json:"iowait_cores"`
	IdleCores   float64 `json:"idle_cores"`
}

// Load is /proc/loadavg: the average number of runnable and uninterruptible
// tasks over 1, 5 and 15 minutes.
type Load struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// Memory is the machine's memory from /proc/meminfo.
type Memory struct {
	TotalBytes uint64 `json:"total_bytes"`
	// AvailableBytes is the kernel's estimate of what can be allocated
	// without swapping, left out by kernels that do not report it.
	AvailableBytes *uint64 `json:"available_bytes,omitempty"`
}

// Cgroup describes the cgroup (v2) the process runs in: under systemd, the
// slot's service unit.
type Cgroup struct {
	// Path is the cgroup's path, e.g. /system.slice/clirelay2-8319.service.
	Path   string        `json:"path"`
	CPU    *CgroupCPU    `json:"cpu,omitempty"`
	Memory *CgroupMemory `json:"memory,omitempty"`
}

// CgroupCPU is the cgroup's cpu.stat counters, its cpu.max quota and the
// rates over the last sample window. The usage includes time stolen by the
// hypervisor (see the package comment).
type CgroupCPU struct {
	UsageUsec     uint64 `json:"usage_usec"`
	NrPeriods     uint64 `json:"nr_periods"`
	NrThrottled   uint64 `json:"nr_throttled"`
	ThrottledUsec uint64 `json:"throttled_usec"`
	// QuotaCores is cpu.max in cores, e.g. 7 for "700000 100000"; left out
	// when the cgroup has no quota.
	QuotaCores *float64 `json:"quota_cores,omitempty"`
	// Window is left out until there are two samples of the same cgroup.
	Window *CgroupCPUWindow `json:"window,omitempty"`
}

// CgroupCPUWindow is the cgroup's CPU over the last sample window.
type CgroupCPUWindow struct {
	Seconds    float64 `json:"seconds"`
	UsageCores float64 `json:"usage_cores"`
	// ThrottledRatio is the share of the quota's enforcement periods in which
	// the cgroup ran out of quota.
	ThrottledRatio float64 `json:"throttled_ratio"`
	// ThrottledSeconds is how long its threads were held back by the quota.
	ThrottledSeconds float64 `json:"throttled_seconds"`
}

// CgroupMemory is the cgroup's memory.current, memory.high and memory.max.
// The limits are left out when they are "max" (none).
type CgroupMemory struct {
	CurrentBytes uint64  `json:"current_bytes"`
	HighBytes    *uint64 `json:"high_bytes,omitempty"`
	MaxBytes     *uint64 `json:"max_bytes,omitempty"`
}

// Traffic is what the process's HTTP listener handled.
type Traffic struct {
	// InFlight counts the HTTP requests being handled now.
	InFlight int64 `json:"in_flight"`
	// WindowSeconds is the span of the counts below: about the last minute,
	// or the time since the process started when it is younger.
	WindowSeconds float64 `json:"window_seconds"`
	Requests      uint64  `json:"requests"`
	// BytesIn and BytesOut count what crossed the listening socket, headers
	// and WebSocket frames included: on a node, the traffic exchanged with
	// the local nginx in front of the slot.
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

// Egress is the state of the proxies upstream traffic leaves through.
type Egress struct {
	// BreakerEnabled is false when CLIRELAY_EGRESS_BREAKER turns the egress
	// breaker off on this node.
	BreakerEnabled bool `json:"breaker_enabled"`
	// BreakerOpen counts the proxies requests are refused for now; the two
	// fields after it split that count by fault.
	BreakerOpen  int `json:"breaker_open"`
	AuthRejected int `json:"auth_rejected"`
	Unreachable  int `json:"unreachable"`
	// OpenSince is when the oldest of those outages began.
	OpenSince *time.Time `json:"open_since,omitempty"`
	// Check is the latest round of the connect check behind /readyz/egress,
	// left out until the first round has finished.
	Check *EgressCheck `json:"check,omitempty"`
}

// EgressCheck is one round of the egress connect check.
type EgressCheck struct {
	Total       int `json:"total"`
	Unreachable int `json:"unreachable"`
}

// RoundTrip is the latest round trip to a dependency.
type RoundTrip struct {
	// Role says which Redis was measured: "cluster" for the shared cluster
	// Redis, "usage" for usage persistence. Empty for the database.
	Role string `json:"role,omitempty"`
	// RTTMillis is set when the round trip succeeded.
	RTTMillis *float64 `json:"rtt_ms,omitempty"`
	// Error is "timeout" or "error" when it failed. The cause stays out of
	// the snapshot, which is stored and shown: driver errors name hosts and
	// users.
	Error string `json:"error,omitempty"`
}
