package servicehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	servicehostv1 "github.com/raulfrk/herdr-plugin-kit/runtime/servicehost/internal/servicehostv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

func TestLifecycleConfigRejectsEachInvalidDimension(t *testing.T) {
	lifecycle, runner, _ := lifecycleFixture(t)
	valid := lifecycle.config
	clone := func() LifecycleConfig {
		config := valid
		config.Paths.PurgePaths = append([]string(nil), valid.Paths.PurgePaths...)
		return config
	}
	tests := []struct {
		name   string
		mutate func(*LifecycleConfig)
	}{
		{"empty unit", func(c *LifecycleConfig) { c.UnitName = "" }},
		{"nested unit", func(c *LifecycleConfig) { c.UnitName = "nested/test.service" }},
		{"unit suffix", func(c *LifecycleConfig) {
			c.UnitName = "test.socket"
			c.Paths.UnitPath = filepath.Join(filepath.Dir(c.Paths.UnitPath), c.UnitName)
		}},
		{"empty service", func(c *LifecycleConfig) { c.ServiceName = "" }},
		{"empty description", func(c *LifecycleConfig) { c.Description = "" }},
		{"long description", func(c *LifecycleConfig) { c.Description = strings.Repeat("x", 257) }},
		{"invalid description utf8", func(c *LifecycleConfig) { c.Description = string([]byte{0xff}) }},
		{"description newline", func(c *LifecycleConfig) { c.Description = "bad\nvalue" }},
		{"zero protocol", func(c *LifecycleConfig) { c.ProtocolVersion = 0 }},
		{"empty client", func(c *LifecycleConfig) { c.ClientName = "" }},
		{"command timeout", func(c *LifecycleConfig) { c.CommandTimeout = -1 }},
		{"readiness timeout", func(c *LifecycleConfig) { c.ReadinessTimeout = -1 }},
		{"drain timeout", func(c *LifecycleConfig) { c.DrainTimeout = -1 }},
		{"relative unit", func(c *LifecycleConfig) { c.Paths.UnitPath = "test.service" }},
		{"unit basename", func(c *LifecycleConfig) {
			c.Paths.UnitPath = filepath.Join(filepath.Dir(c.Paths.UnitPath), "other.service")
		}},
		{"relative descriptor", func(c *LifecycleConfig) { c.Paths.DescriptorPath = "service.json" }},
		{"relative binary", func(c *LifecycleConfig) { c.Paths.BinaryDirectory = "service" }},
		{"root binary", func(c *LifecycleConfig) { c.Paths.BinaryDirectory = "/" }},
		{"relative runtime", func(c *LifecycleConfig) { c.Paths.RuntimeDirectory = "runtime" }},
		{"root runtime", func(c *LifecycleConfig) { c.Paths.RuntimeDirectory = "/" }},
		{"relative socket", func(c *LifecycleConfig) { c.Paths.SocketPath = "service.sock" }},
		{"socket parent", func(c *LifecycleConfig) {
			c.Paths.SocketPath = filepath.Join(filepath.Dir(c.Paths.RuntimeDirectory), "service.sock")
		}},
		{"runtime suffix", func(c *LifecycleConfig) { c.Paths.RuntimeDirectory += "-other" }},
		{"descriptor root parent", func(c *LifecycleConfig) { c.Paths.DescriptorPath = "/service.json" }},
		{"unit root parent", func(c *LifecycleConfig) { c.Paths.UnitPath = "/" + c.UnitName }},
	}
	for _, value := range []string{"", "/absolute", ".", "..", "../other", "bad%name", "bad$name", "bad\x00name", "bad\nname"} {
		value := value
		tests = append(tests, struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"runtime name " + fmt.Sprintf("%q", value), func(c *LifecycleConfig) { c.Paths.RuntimeDirectoryName = value }})
	}
	tests = append(tests,
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"too many purge paths", func(c *LifecycleConfig) {
			c.Paths.PurgePaths = make([]string, 65)
			for index := range c.Paths.PurgePaths {
				c.Paths.PurgePaths[index] = filepath.Join(filepath.Dir(c.Paths.BinaryDirectory), fmt.Sprintf("purge-%d", index))
			}
		}},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"relative purge", func(c *LifecycleConfig) { c.Paths.PurgePaths = []string{"state"} }},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"root purge", func(c *LifecycleConfig) { c.Paths.PurgePaths = []string{"/"} }},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"purge contains descriptor", func(c *LifecycleConfig) { c.Paths.PurgePaths = []string{filepath.Dir(c.Paths.DescriptorPath)} }},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"purge inside binary", func(c *LifecycleConfig) {
			c.Paths.PurgePaths = []string{filepath.Join(c.Paths.BinaryDirectory, "state")}
		}},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"duplicate purge", func(c *LifecycleConfig) {
			path := filepath.Join(filepath.Dir(c.Paths.BinaryDirectory), "state")
			c.Paths.PurgePaths = []string{path, path}
		}},
		struct {
			name   string
			mutate func(*LifecycleConfig)
		}{"nested purge", func(c *LifecycleConfig) {
			path := filepath.Join(filepath.Dir(c.Paths.BinaryDirectory), "state")
			c.Paths.PurgePaths = []string{path, filepath.Join(path, "nested")}
		}},
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := clone()
			test.mutate(&config)
			if _, err := NewLifecycle(config, runner); err == nil {
				t.Fatal("invalid lifecycle configuration was accepted")
			}
		})
	}
}

func TestLifecycleDescriptorRejectsEachInvalidDimension(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	descriptor, source, err := lifecycle.descriptorForInstall(request)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	_ = source.Close()
	if !lifecycle.validDescriptor(descriptor) {
		t.Fatal("valid descriptor rejected")
	}
	release := func(version string) lifecycleRelease {
		return lifecycleRelease{
			Executable:       filepath.Join(lifecycle.config.Paths.BinaryDirectory, version, "service"),
			ExecutableSHA256: strings.Repeat("a", sha256.Size*2), Version: version,
			Arguments: []string{"serve"},
		}
	}
	tests := []struct {
		name   string
		mutate func(*lifecycleDescriptor)
	}{
		{"schema", func(d *lifecycleDescriptor) { d.SchemaVersion++ }},
		{"unit", func(d *lifecycleDescriptor) { d.UnitName = "other.service" }},
		{"service", func(d *lifecycleDescriptor) { d.ServiceName = "other" }},
		{"unit path", func(d *lifecycleDescriptor) { d.UnitPath += ".other" }},
		{"protocol", func(d *lifecycleDescriptor) { d.ProtocolVersion++ }},
		{"runtime", func(d *lifecycleDescriptor) { d.RuntimeDirectory += ".other" }},
		{"runtime name", func(d *lifecycleDescriptor) { d.RuntimeDirectoryName += "-other" }},
		{"socket", func(d *lifecycleDescriptor) { d.SocketPath += ".other" }},
		{"purge paths", func(d *lifecycleDescriptor) { d.PurgePaths = []string{"/different"} }},
		{"relative executable", func(d *lifecycleDescriptor) { d.Executable = "service" }},
		{"wrong executable", func(d *lifecycleDescriptor) { d.Executable += ".other" }},
		{"invalid version", func(d *lifecycleDescriptor) { d.Version = "bad/version" }},
		{"digest length", func(d *lifecycleDescriptor) { d.ExecutableSHA256 = "short" }},
		{"argument", func(d *lifecycleDescriptor) { d.Arguments = []string{"bad\nargument"} }},
		{"previous invalid", func(d *lifecycleDescriptor) {
			previous := release("0.9.0")
			previous.Executable = "relative"
			d.Previous = &previous
		}},
		{"previous current", func(d *lifecycleDescriptor) { current := releaseFromDescriptor(*d); d.Previous = &current }},
		{"pending invalid", func(d *lifecycleDescriptor) {
			pending := release("2.0.0")
			pending.Executable = "relative"
			d.Pending = &pending
		}},
		{"pending current", func(d *lifecycleDescriptor) { current := releaseFromDescriptor(*d); d.Pending = &current }},
		{"same previous pending", func(d *lifecycleDescriptor) {
			previous := release("2.0.0")
			pending := previous
			d.Previous, d.Pending = &previous, &pending
		}},
		{"transition without pending", func(d *lifecycleDescriptor) {
			d.Transition = &lifecycleTransition{Phase: upgradeStoppingPrevious, Previous: releaseFromDescriptor(*d), Candidate: release("2.0.0")}
		}},
		{"negative transition pid", func(d *lifecycleDescriptor) {
			pending := release("2.0.0")
			d.Pending = &pending
			d.Transition = &lifecycleTransition{Phase: upgradeStoppingPrevious, Previous: releaseFromDescriptor(*d), Candidate: pending, SourcePID: -1}
		}},
		{"transition previous", func(d *lifecycleDescriptor) {
			pending := release("2.0.0")
			d.Pending = &pending
			d.Transition = &lifecycleTransition{Phase: upgradeStoppingPrevious, Previous: release("0.9.0"), Candidate: pending}
		}},
		{"transition candidate", func(d *lifecycleDescriptor) {
			pending := release("2.0.0")
			d.Pending = &pending
			d.Transition = &lifecycleTransition{Phase: upgradeStoppingPrevious, Previous: releaseFromDescriptor(*d), Candidate: release("3.0.0")}
		}},
		{"transition phase", func(d *lifecycleDescriptor) {
			pending := release("2.0.0")
			d.Pending = &pending
			d.Transition = &lifecycleTransition{Phase: "invalid", Previous: releaseFromDescriptor(*d), Candidate: pending}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := descriptor
			candidate.Arguments = append([]string(nil), descriptor.Arguments...)
			test.mutate(&candidate)
			if lifecycle.validDescriptor(candidate) {
				t.Fatal("invalid descriptor was accepted")
			}
		})
	}
	for _, phase := range []string{upgradeStoppingPrevious, upgradePreviousStopped, upgradeSwitchingUnit, upgradeCandidateUnit, upgradeCandidateStarted} {
		candidate := descriptor
		pending := release("2.0.0")
		candidate.Pending = &pending
		candidate.Transition = &lifecycleTransition{Phase: phase, Previous: releaseFromDescriptor(descriptor), Candidate: pending}
		if !lifecycle.validDescriptor(candidate) {
			t.Fatalf("valid transition phase %q rejected", phase)
		}
	}
}

func TestLifecyclePureValidationBoundaries(t *testing.T) {
	if !validLifecycleDescriptorSize(make([]byte, maxLifecycleDescriptor)) || validLifecycleDescriptorSize(make([]byte, maxLifecycleDescriptor+1)) {
		t.Fatal("lifecycle descriptor size boundary changed")
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"a", true}, {"z", true}, {"A", true}, {"Z", true}, {"0", true}, {"9", true}, {"._+-", true},
		{"", false}, {".", false}, {"..", false},
		{strings.Repeat("a", MaxIdentityBytes), true}, {strings.Repeat("a", MaxIdentityBytes+1), false},
		{"bad/slash", false}, {"bad space", false}, {string([]byte{0xff}), false},
		{"`", false}, {"{", false}, {"@", false}, {"[", false}, {"/", false}, {":", false},
	} {
		if got := validLifecycleToken(test.value); got != test.valid {
			t.Fatalf("validLifecycleToken(%q)=%t, want %t", test.value, got, test.valid)
		}
	}
	if !sameProcess(LifecycleStatus{Active: true, MainPID: 7}, LifecycleStatus{Active: true, MainPID: 7}) {
		t.Fatal("same active process rejected")
	}
	for _, current := range []LifecycleStatus{
		{Active: false, MainPID: 7}, {Active: true, MainPID: 0}, {Active: true, MainPID: 8},
	} {
		if sameProcess(current, LifecycleStatus{Active: true, MainPID: 7}) {
			t.Fatalf("different process accepted: %+v", current)
		}
	}
	cause := errors.New("status failed")
	if got := terminalStatusError(LifecycleStatus{}, cause, "message"); !errors.Is(got, cause) {
		t.Fatalf("status cause lost: %v", got)
	}
	if got := terminalStatusError(LifecycleStatus{ActiveState: "inactive"}, nil, "message"); got != nil {
		t.Fatalf("terminal status rejected: %v", got)
	}
	if got := terminalStatusError(LifecycleStatus{ActiveState: "deactivating", MainPID: 7}, nil, "message"); got == nil || !strings.Contains(got.Error(), "state=deactivating pid=7") {
		t.Fatalf("nonterminal status error = %v", got)
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"runtime", true}, {"nested/runtime", true}, {"", false}, {"/runtime", false}, {".", false}, {"..", false},
		{"../runtime", false}, {"bad%name", false}, {"bad$name", false}, {"bad\x00name", false}, {"bad\nname", false},
	} {
		if got := validRuntimeDirectoryName(test.value); got != test.valid {
			t.Fatalf("validRuntimeDirectoryName(%q)=%t, want %t", test.value, got, test.valid)
		}
	}
	for _, test := range []struct {
		parent, child string
		contains      bool
	}{
		{"/a", "/a", true}, {"/a", "/a/b", true}, {"/a", "/ab", false}, {"/a/b", "/a", false},
	} {
		if got := lifecyclePathContains(test.parent, test.child); got != test.contains {
			t.Fatalf("lifecyclePathContains(%q,%q)=%t, want %t", test.parent, test.child, got, test.contains)
		}
	}
}

func TestLifecycleConfigAcceptsExactBoundaries(t *testing.T) {
	lifecycle, runner, _ := lifecycleFixture(t)
	config := lifecycle.config
	config.ServiceName = strings.Repeat("s", MaxIdentityBytes)
	config.ClientName = strings.Repeat("c", MaxIdentityBytes)
	config.Description = strings.Repeat("d", 256)
	config.CommandTimeout = 1
	config.ReadinessTimeout = 1
	config.DrainTimeout = 1
	config.Paths.PurgePaths = make([]string, 64)
	for index := range config.Paths.PurgePaths {
		config.Paths.PurgePaths[index] = filepath.Join(filepath.Dir(config.Paths.BinaryDirectory), fmt.Sprintf("boundary-%d", index))
	}
	if _, err := NewLifecycle(config, runner); err != nil {
		t.Fatalf("exact lifecycle boundaries rejected: %v", err)
	}
}

func TestParseLifecycleStatusRejectsEveryMalformedDimension(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	descriptor, source, err := lifecycle.descriptorForInstall(request)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	_ = source.Close()
	valid := lifecycleShowOutput(lifecycle, "active", "running", "enabled-runtime", 42)
	status, err := parseLifecycleStatus(valid, descriptor)
	if err != nil || !status.Installed || !status.Enabled || !status.Active || status.MainPID != 42 {
		t.Fatalf("valid status = %+v, %v", status, err)
	}
	for _, field := range []string{"Id", "LoadState", "ActiveState", "SubState", "Result", "MainPID", "FragmentPath", "UnitFileState"} {
		lines := strings.Split(strings.TrimSpace(string(valid)), "\n")
		filtered := lines[:0]
		for _, line := range lines {
			if !strings.HasPrefix(line, field+"=") {
				filtered = append(filtered, line)
			}
		}
		if _, err := parseLifecycleStatus([]byte(strings.Join(filtered, "\n")), descriptor); err == nil {
			t.Fatalf("missing field %s accepted", field)
		}
	}
	invalid := [][]byte{
		bytes.Replace(valid, []byte("MainPID=42"), []byte("MainPID=bad"), 1),
		bytes.Replace(valid, []byte("MainPID=42"), []byte("MainPID=-1"), 1),
		bytes.Replace(valid, []byte("Id="+descriptor.UnitName), []byte("Id=other.service"), 1),
		append(append([]byte(nil), valid...), []byte("Id="+descriptor.UnitName+"\n")...),
		[]byte("not-a-field"),
	}
	for index, raw := range invalid {
		if _, err := parseLifecycleStatus(raw, descriptor); err == nil {
			t.Fatalf("invalid status %d accepted", index)
		}
	}
	notFound := lifecycleNotFoundOutput(lifecycle)
	status, err = parseLifecycleStatus(notFound, descriptor)
	if err != nil || status.Active || status.Enabled || status.LoadState != "not-found" {
		t.Fatalf("not-found status = %+v, %v", status, err)
	}
}

func TestUpgradeReleaseCopiesDoNotAliasArguments(t *testing.T) {
	left := lifecycleRelease{Executable: "/x", ExecutableSHA256: "hash", Version: "1", Arguments: []string{"a"}}
	right := *cloneLifecycleRelease(left)
	if !sameLifecycleRelease(left, right) {
		t.Fatal("equal release rejected")
	}
	right.Arguments[0] = "b"
	if left.Arguments[0] != "a" || sameLifecycleRelease(left, right) {
		t.Fatal("release clone aliases arguments or equality ignored arguments")
	}
	for _, mutate := range []func(*lifecycleRelease){
		func(r *lifecycleRelease) { r.Executable += "x" },
		func(r *lifecycleRelease) { r.ExecutableSHA256 += "x" },
		func(r *lifecycleRelease) { r.Version += "x" },
	} {
		candidate := left
		mutate(&candidate)
		if sameLifecycleRelease(left, candidate) {
			t.Fatal("different release accepted as equal")
		}
	}
	public := publicUpgradeRelease(left)
	public.Arguments[0] = "changed"
	if left.Arguments[0] != "a" {
		t.Fatal("public release aliases internal arguments")
	}
}

func TestLifecycleTimeoutDefaultsRemainPositive(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if lifecycle.config.CommandTimeout <= 0 || lifecycle.config.ReadinessTimeout <= 0 || lifecycle.config.DrainTimeout <= 0 {
		t.Fatalf("invalid normalized timeouts: %+v", lifecycle.config)
	}
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	cancel()
	if _, err := lifecycle.Status(ctx); err == nil {
		t.Fatal("canceled status unexpectedly succeeded")
	}
}

func TestLifecycleInternalTimeoutCalculations(t *testing.T) {
	lifecycle, _, _ := lifecycleFixture(t)
	lifecycle.config.CommandTimeout = 3 * time.Second
	lifecycle.config.ReadinessTimeout = 7 * time.Second
	if got := lifecycle.previousRestoreTimeout(); got != 19*time.Second {
		t.Fatalf("previous restore timeout = %v", got)
	}
	if got := lifecycle.rollbackCleanupTimeout(); got != 22*time.Second {
		t.Fatalf("rollback cleanup timeout = %v", got)
	}
	if got := lifecycle.finalStopTimeout(); got != 6*time.Second {
		t.Fatalf("final stop timeout = %v", got)
	}
	lifecycle.upgradeCleanupTimeout = func() time.Duration { return 11 * time.Second }
	if got := lifecycle.rollbackCleanupTimeout(); got != 11*time.Second {
		t.Fatalf("overridden rollback timeout = %v", got)
	}
	if lifecycleDrainDialTimeout != 2*time.Second || lifecycleReadyDialTimeout != 250*time.Millisecond || lifecycleReadyPollInterval != 25*time.Millisecond {
		t.Fatal("lifecycle transport timing constants changed")
	}
}

func TestServerConfigNormalizationRejectsEveryInvalidDimension(t *testing.T) {
	valid := ServerConfig{
		SocketPath: "/tmp/servicehost-test.sock", ProtocolVersion: 1,
		ServiceName: "service", ServiceVersion: "1.0.0",
	}
	normalized := valid
	if err := normalizeServerConfig(&normalized); err != nil {
		t.Fatalf("normalize valid: %v", err)
	}
	if normalized.MaxMessageBytes != DefaultMaxMessageBytes || normalized.MaxConcurrentRPC != DefaultMaxConcurrentRPC ||
		normalized.MaxWatchers != DefaultMaxWatchers || normalized.RPCTimeout != DefaultRPCTimeout ||
		normalized.DrainTimeout != DefaultDrainTimeout || normalized.ResumeTimeout != DefaultResumeTimeout ||
		normalized.ShutdownTimeout != DefaultShutdownTimeout || normalized.InitialState != HealthStarting {
		t.Fatalf("unexpected defaults: %+v", normalized)
	}
	exactOne := valid
	exactOne.MaxMessageBytes = 1
	exactOne.MaxConcurrentRPC = 1
	exactOne.MaxWatchers = 1
	exactOne.RPCTimeout = 1
	exactOne.DrainTimeout = 1
	exactOne.ResumeTimeout = 1
	exactOne.ShutdownTimeout = 1
	exactOne.InitialState = HealthStopping
	if err := normalizeServerConfig(&exactOne); err != nil {
		t.Fatalf("exact-one bounds rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ServerConfig)
	}{
		{"socket", func(c *ServerConfig) { c.SocketPath = "relative" }},
		{"protocol", func(c *ServerConfig) { c.ProtocolVersion = 0 }},
		{"service name", func(c *ServerConfig) { c.ServiceName = "" }},
		{"service version", func(c *ServerConfig) { c.ServiceVersion = "" }},
		{"message bytes", func(c *ServerConfig) { c.MaxMessageBytes = -1 }},
		{"rpc concurrency", func(c *ServerConfig) { c.MaxConcurrentRPC = -1 }},
		{"watchers", func(c *ServerConfig) { c.MaxWatchers = -1 }},
		{"rpc timeout", func(c *ServerConfig) { c.RPCTimeout = -1 }},
		{"drain timeout", func(c *ServerConfig) { c.DrainTimeout = -1 }},
		{"resume timeout", func(c *ServerConfig) { c.ResumeTimeout = -1 }},
		{"shutdown timeout", func(c *ServerConfig) { c.ShutdownTimeout = -1 }},
		{"initial state", func(c *ServerConfig) { c.InitialState = HealthStopping + 1 }},
		{"initial detail", func(c *ServerConfig) { c.InitialDetail = strings.Repeat("x", MaxStatusDetailBytes+1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := normalizeServerConfig(&candidate); err == nil {
				t.Fatal("invalid server configuration accepted")
			}
		})
	}
}

func TestClientConfigRejectsEveryInvalidDimension(t *testing.T) {
	valid := ClientConfig{
		SocketPath: "/tmp/servicehost-test.sock", ProtocolVersion: 1, ClientName: "client",
		MaxMessageBytes: 1, DialTimeout: 1, RPCTimeout: 1, DrainTimeout: 1,
	}
	tests := []struct {
		name   string
		mutate func(*ClientConfig)
	}{
		{"socket", func(c *ClientConfig) { c.SocketPath = "relative" }},
		{"protocol", func(c *ClientConfig) { c.ProtocolVersion = 0 }},
		{"client", func(c *ClientConfig) { c.ClientName = "" }},
		{"message bytes", func(c *ClientConfig) { c.MaxMessageBytes = -1 }},
		{"dial timeout", func(c *ClientConfig) { c.DialTimeout = -1 }},
		{"rpc timeout", func(c *ClientConfig) { c.RPCTimeout = -1 }},
		{"drain timeout", func(c *ClientConfig) { c.DrainTimeout = -1 }},
		{"invalid utf8", func(c *ClientConfig) { c.ClientName = string([]byte{0xff}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			_, err := Dial(context.Background(), candidate)
			if err == nil {
				t.Fatal("invalid client configuration accepted")
			}
			want := "servicehost: invalid client bounds"
			if test.name == "socket" || test.name == "protocol" || test.name == "client" || test.name == "invalid utf8" {
				want = "servicehost: valid socket, protocol version, and client name are required"
			}
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
		})
	}
	for _, mutate := range []func(*ClientConfig){
		func(c *ClientConfig) { c.MaxMessageBytes = 1 },
		func(c *ClientConfig) { c.DialTimeout = 1 },
		func(c *ClientConfig) { c.RPCTimeout = 1 },
		func(c *ClientConfig) { c.DrainTimeout = 1 },
	} {
		candidate := valid
		candidate.MaxMessageBytes = DefaultMaxMessageBytes
		candidate.DialTimeout = time.Millisecond
		candidate.RPCTimeout = time.Millisecond
		candidate.DrainTimeout = time.Millisecond
		mutate(&candidate)
		_, err := Dial(context.Background(), candidate)
		if err == nil || err.Error() == "servicehost: invalid client bounds" {
			t.Fatalf("exact-one client bound rejected as invalid: %v", err)
		}
	}
}

func TestIdentityAndStatusValidationBoundaries(t *testing.T) {
	if !validIdentity("a") || !validIdentity(strings.Repeat("x", MaxIdentityBytes)) {
		t.Fatal("valid identity rejected")
	}
	for _, value := range []string{"", strings.Repeat("x", MaxIdentityBytes+1), string([]byte{0xff})} {
		if validIdentity(value) {
			t.Fatalf("invalid identity %q accepted", value)
		}
	}
	if err := validateStatus(HealthStarting, ""); err != nil {
		t.Fatalf("starting state rejected: %v", err)
	}
	if err := validateStatus(HealthStopping, ""); err != nil {
		t.Fatalf("stopping state rejected: %v", err)
	}
	for _, state := range []HealthState{0, HealthStopping + 1} {
		if err := validateStatus(state, ""); err == nil {
			t.Fatalf("invalid health state %d accepted", state)
		}
	}
	if err := validateStatus(HealthReady, strings.Repeat("x", MaxStatusDetailBytes)); err != nil {
		t.Fatalf("exact-limit detail rejected: %v", err)
	}
	for _, detail := range []string{strings.Repeat("x", MaxStatusDetailBytes+1), string([]byte{0xff})} {
		if err := validateStatus(HealthReady, detail); err == nil {
			t.Fatal("invalid status detail accepted")
		}
	}
}

func TestStatusHubEveryTransitionAndClosedState(t *testing.T) {
	hub, err := newStatusHub(HealthStarting, false, "starting", 2)
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	initial := hub.current()
	unchanged, err := hub.update(initial.State, initial.Accepting, initial.Detail)
	if err != nil || unchanged.Revision != initial.Revision {
		t.Fatalf("unchanged update = %+v, %v", unchanged, err)
	}
	for index, update := range []struct {
		state     HealthState
		accepting bool
		detail    string
	}{
		{HealthReady, false, "starting"},
		{HealthReady, true, "starting"},
		{HealthReady, true, "ready"},
	} {
		got, err := hub.update(update.state, update.accepting, update.detail)
		if err != nil || got.Revision != initial.Revision+uint64(index)+1 || got.State != update.state || got.Accepting != update.accepting || got.Detail != update.detail {
			t.Fatalf("update %d = %+v, %v", index, got, err)
		}
	}
	current := hub.current()
	id1, ch1, reset, err := hub.subscribe(Cursor{Epoch: current.Epoch, Revision: current.Revision})
	if err != nil || reset != nil || id1 == 0 || ch1 == nil {
		t.Fatalf("exact subscribe = id%d reset%+v err%v", id1, reset, err)
	}
	id2, _, reset, err := hub.subscribe(Cursor{Epoch: "other", Revision: current.Revision})
	if err != nil || reset == nil || !reset.Reset || reset.Status != current || id2 != id1+1 {
		t.Fatalf("epoch reset subscribe = id%d reset%+v err%v", id2, reset, err)
	}
	if _, _, _, err := hub.subscribe(Cursor{}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("watcher bound error = %v", err)
	}
	hub.unsubscribe(id1)
	hub.unsubscribe(id1)
	id3, _, reset, err := hub.subscribe(Cursor{Epoch: current.Epoch, Revision: current.Revision - 1})
	if err != nil || reset == nil || !reset.Reset || id3 != id2+1 {
		t.Fatalf("revision reset = %+v, %v", reset, err)
	}
	hub.close()
	hub.close()
	if _, err := hub.update(HealthDegraded, false, "closed"); err == nil {
		t.Fatal("closed hub update succeeded")
	}
	if _, _, _, err := hub.subscribe(Cursor{}); err == nil {
		t.Fatal("closed hub subscribe succeeded")
	}
}

func TestLifecycleReleaseAndTransitionSelectionMatrix(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	descriptor, source, err := lifecycle.descriptorForInstall(request)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	_ = source.Close()
	previous := releaseFromDescriptor(descriptor)
	candidate := previous
	candidate.Version = "2.0.0"
	candidate.Executable = filepath.Join(lifecycle.config.Paths.BinaryDirectory, candidate.Version, "service")
	descriptor.Pending = cloneLifecycleRelease(candidate)
	if releases := uniqueLifecycleReleases(descriptor); len(releases) != 2 || releases[0].Version != previous.Version || releases[1].Version != candidate.Version {
		t.Fatalf("unique releases = %+v", releases)
	}
	descriptor.Previous = cloneLifecycleRelease(candidate)
	if releases := uniqueLifecycleReleases(descriptor); len(releases) != 2 {
		t.Fatalf("duplicate releases = %+v", releases)
	}
	for _, test := range []struct {
		name          string
		transition    *lifecycleTransition
		status        LifecycleStatus
		wantCandidate bool
	}{
		{"none", nil, LifecycleStatus{MainPID: 2}, false},
		{"source pid", &lifecycleTransition{Phase: upgradeCandidateStarted, SourcePID: 2, Previous: previous, Candidate: candidate}, LifecycleStatus{MainPID: 2}, false},
		{"stopping", &lifecycleTransition{Phase: upgradeStoppingPrevious, SourcePID: 1, Previous: previous, Candidate: candidate}, LifecycleStatus{MainPID: 2}, false},
		{"previous stopped", &lifecycleTransition{Phase: upgradePreviousStopped, SourcePID: 1, Previous: previous, Candidate: candidate}, LifecycleStatus{MainPID: 2}, false},
		{"candidate", &lifecycleTransition{Phase: upgradeCandidateStarted, SourcePID: 1, Previous: previous, Candidate: candidate}, LifecycleStatus{MainPID: 2}, true},
	} {
		descriptor.Transition = test.transition
		if got := runningReleaseIsCurrent(test.transition, test.status); got == test.wantCandidate {
			t.Fatalf("%s current-release classification = %t", test.name, got)
		}
		got := descriptorForRunningTransition(descriptor, test.status)
		if (got.Version == candidate.Version) != test.wantCandidate {
			t.Fatalf("%s selected version %s", test.name, got.Version)
		}
	}
	for _, test := range []struct {
		status   LifecycleStatus
		terminal bool
	}{
		{LifecycleStatus{ActiveState: "inactive"}, true},
		{LifecycleStatus{ActiveState: "failed"}, true},
		{LifecycleStatus{ActiveState: "active", Active: true, MainPID: 1}, false},
		{LifecycleStatus{ActiveState: "inactive", MainPID: 1}, false},
		{LifecycleStatus{ActiveState: "deactivating"}, false},
	} {
		if got := terminalStopped(test.status); got != test.terminal {
			t.Fatalf("terminalStopped(%+v)=%t", test.status, got)
		}
	}
	for _, test := range []struct {
		status LifecycleStatus
		active bool
	}{
		{LifecycleStatus{Active: true, MainPID: 1}, true},
		{LifecycleStatus{Active: false, MainPID: 1}, false},
		{LifecycleStatus{Active: true, MainPID: 0}, false},
		{LifecycleStatus{Active: true, MainPID: -1}, false},
	} {
		if got := activeProcess(test.status); got != test.active {
			t.Fatalf("activeProcess(%+v)=%t", test.status, got)
		}
	}
	for _, test := range []struct {
		status LifecycleStatus
		absent bool
	}{
		{LifecycleStatus{LoadState: "not-found"}, true},
		{LifecycleStatus{LoadState: "loaded"}, false},
		{LifecycleStatus{LoadState: "not-found", Active: true}, false},
		{LifecycleStatus{LoadState: "not-found", MainPID: 1}, false},
	} {
		if got := managerAbsent(test.status); got != test.absent {
			t.Fatalf("managerAbsent(%+v)=%t", test.status, got)
		}
	}
	for _, test := range []struct {
		status LifecycleStatus
		safe   bool
	}{
		{LifecycleStatus{LoadState: "not-found", ActiveState: "inactive"}, true},
		{LifecycleStatus{LoadState: "loaded", ActiveState: "inactive"}, false},
		{LifecycleStatus{LoadState: "not-found", ActiveState: "active", Active: true, MainPID: 1}, false},
	} {
		if got := safeUnloadedManager(test.status); got != test.safe {
			t.Fatalf("safeUnloadedManager(%+v)=%t", test.status, got)
		}
	}
	if minDuration(time.Second, 2*time.Second) != time.Second || minDuration(2*time.Second, time.Second) != time.Second {
		t.Fatal("minDuration returned wrong operand")
	}
}

func TestRemoveVerifiedExecutablePropagatesDirectoryRemovalFailure(t *testing.T) {
	parent := t.TempDir()
	versionDirectory := filepath.Join(parent, "v1")
	if err := os.Mkdir(versionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	removed, err := removeVerifiedExecutable(lifecycleRelease{Executable: filepath.Join(versionDirectory, "service")})
	if err == nil || removed {
		t.Fatalf("remove unavailable directory = %t, %v", removed, err)
	}
}

func TestLifecycleFilesystemValidationDimensions(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := ensureLifecycleDirectory(private, true, uint32(os.Getuid())); err != nil {
		t.Fatalf("ensure private: %v", err)
	}
	if err := validateLifecycleDirectory(private, true, uint32(os.Getuid())); err != nil {
		t.Fatalf("validate private: %v", err)
	}
	if err := os.Chmod(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateLifecycleDirectory(private, true, uint32(os.Getuid())); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("private mode error = %v", err)
	}
	if err := validateLifecycleDirectory(private, false, uint32(os.Getuid())); err != nil {
		t.Fatalf("non-private directory rejected: %v", err)
	}
	filePath := filepath.Join(root, "file")
	contents := []byte("contents")
	if err := os.WriteFile(filePath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyLifecycleFile(filePath, contents, 0o600); err != nil {
		t.Fatalf("verify file: %v", err)
	}
	for _, mutate := range []func() func(){
		func() func() { _ = os.Chmod(filePath, 0o644); return func() { _ = os.Chmod(filePath, 0o600) } },
		func() func() {
			_ = os.WriteFile(filePath, []byte("changed"), 0o600)
			return func() { _ = os.WriteFile(filePath, contents, 0o600) }
		},
	} {
		restore := mutate()
		if err := verifyLifecycleFile(filePath, contents, 0o600); !errors.Is(err, ErrLifecycleOwnership) {
			t.Fatalf("changed file error = %v", err)
		}
		restore()
	}
	if err := verifyLifecycleFile(root, nil, 0o700); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("directory verified as file: %v", err)
	}
	info1, err := os.Lstat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.WriteFile(other, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	fake := syntheticFileInfo{mode: 0o600, sys: "not-stat"}
	if !ownedByUID(info1, uint32(os.Getuid())) || ownedByUID(info1, uint32(os.Getuid()+1)) {
		t.Fatal("ownedByUID mismatch")
	}
	if !ownedRegularFile(info1, uint32(os.Getuid())) || ownedRegularFile(info1, uint32(os.Getuid()+1)) || ownedRegularFile(fake, uint32(os.Getuid())) {
		t.Fatal("ownedRegularFile mismatch")
	}
	if !sameFileIdentity(info1, info1) || sameFileIdentity(info1, info2) {
		t.Fatal("sameFileIdentity mismatch")
	}
	captured, err := inspectCreatedFile(filePath)
	if err != nil || !captured.matches(info1) || captured.matches(info2) {
		t.Fatalf("captured identity mismatch: %v", err)
	}
	if !sameStatIdentity(info1, captured.stat) || sameStatIdentity(info2, captured.stat) || sameStatIdentity(fake, captured.stat) {
		t.Fatal("sameStatIdentity mismatch")
	}
	if ownedByUID(fake, uint32(os.Getuid())) || sameFileIdentity(fake, info1) || sameFileIdentity(info1, fake) || captured.matches(fake) {
		t.Fatal("non-stat file metadata accepted")
	}
	executable := filepath.Join(root, "executable")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("binary")))
	if err := verifyExecutable(executable, digest); err != nil {
		t.Fatalf("verify executable: %v", err)
	}
	if err := os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutable(executable, digest); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("non-executable accepted: %v", err)
	}
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutable(executable, strings.Repeat("0", sha256.Size*2)); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	symlink := filepath.Join(root, "executable-link")
	if err := os.Symlink(executable, symlink); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutable(symlink, digest); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("symlink executable accepted: %v", err)
	}
}

type syntheticFileInfo struct {
	mode os.FileMode
	sys  any
}

func (syntheticFileInfo) Name() string           { return "synthetic" }
func (syntheticFileInfo) Size() int64            { return 0 }
func (info syntheticFileInfo) Mode() os.FileMode { return info.mode }
func (syntheticFileInfo) ModTime() time.Time     { return time.Time{} }
func (info syntheticFileInfo) IsDir() bool       { return info.mode.IsDir() }
func (info syntheticFileInfo) Sys() any          { return info.sys }

func TestLifecycleAndSocketLockIdentityAndBusy(t *testing.T) {
	root := t.TempDir()
	lifecyclePath := filepath.Join(root, "lifecycle.lock")
	if _, err := acquireLifecycleLock(lifecyclePath, uint32(os.Getuid()+1)); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("wrong-uid lifecycle lock error = %v", err)
	}
	first, err := acquireLifecycleLock(lifecyclePath, uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("first lifecycle lock: %v", err)
	}
	if _, err := acquireLifecycleLock(lifecyclePath, uint32(os.Getuid())); !errors.Is(err, ErrLifecycleBusy) {
		t.Fatalf("busy lifecycle lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "socket.lock")
	if _, err := acquireSocketLock(socketPath, uint32(os.Getuid()+1)); !errors.Is(err, ErrSocketOwnership) {
		t.Fatalf("wrong-uid socket lock error = %v", err)
	}
	socketLock, err := acquireSocketLock(socketPath, uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("first socket lock: %v", err)
	}
	if _, err := acquireSocketLock(socketPath, uint32(os.Getuid())); !errors.Is(err, ErrSocketActive) {
		t.Fatalf("busy socket lock error = %v", err)
	}
	if err := socketLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := socketLock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockInstalledRejectsWrongMode(t *testing.T) {
	lifecycle, _, _ := lifecycleFixture(t)
	path := lifecycle.config.Paths.DescriptorPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.lockInstalled(); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("wrong-mode lifecycle lock error = %v", err)
	}
}

func TestTransitionUnitSelectionEveryPhase(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists {
		t.Fatalf("descriptor: exists=%t err=%v", exists, err)
	}
	currentUnit, err := renderLifecycleUnit(lifecycle.config, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	candidate := releaseFromDescriptor(descriptor)
	candidate.Version = "2.0.0"
	candidate.Executable = filepath.Join(lifecycle.config.Paths.BinaryDirectory, candidate.Version, "service")
	candidateDescriptor := descriptorWithRelease(descriptor, candidate)
	candidateUnit, err := renderLifecycleUnit(lifecycle.config, candidateDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	writeUnit := func(contents []byte) {
		if err := os.WriteFile(lifecycle.config.Paths.UnitPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"", upgradeStoppingPrevious, upgradePreviousStopped} {
		candidateDescriptor := descriptor
		if phase != "" {
			candidateDescriptor.Transition = &lifecycleTransition{Phase: phase, Previous: releaseFromDescriptor(descriptor), Candidate: candidate}
		}
		if !transitionUsesCurrentUnit(candidateDescriptor.Transition) {
			t.Fatalf("phase %q did not select current unit", phase)
		}
		writeUnit(currentUnit)
		got, err := expectedUninstallUnit(lifecycle.config, candidateDescriptor)
		if err != nil || !bytes.Equal(got, currentUnit) {
			t.Fatalf("current phase %q = %q, %v", phase, got, err)
		}
		if err := lifecycle.verifyDescriptorUnit(candidateDescriptor); err != nil {
			t.Fatalf("verify current phase %q: %v", phase, err)
		}
	}
	for _, phase := range []string{upgradeSwitchingUnit, upgradeCandidateUnit, upgradeCandidateStarted} {
		withTransition := descriptor
		withTransition.Transition = &lifecycleTransition{Phase: phase, Previous: releaseFromDescriptor(descriptor), Candidate: candidate}
		if transitionUsesCurrentUnit(withTransition.Transition) {
			t.Fatalf("phase %q unexpectedly selected current unit", phase)
		}
		writeUnit(candidateUnit)
		got, err := expectedUninstallUnit(lifecycle.config, withTransition)
		if err != nil || !bytes.Equal(got, candidateUnit) {
			t.Fatalf("candidate phase %q = %q, %v", phase, got, err)
		}
		if err := lifecycle.verifyDescriptorUnit(withTransition); err != nil {
			t.Fatalf("verify candidate phase %q: %v", phase, err)
		}
		if phase == upgradeSwitchingUnit {
			writeUnit(currentUnit)
			if err := lifecycle.verifyDescriptorUnit(withTransition); err != nil {
				t.Fatalf("switching current unit rejected: %v", err)
			}
		}
	}
	writeUnit([]byte("foreign"))
	transition := descriptor
	transition.Transition = &lifecycleTransition{Phase: upgradeCandidateStarted, Previous: releaseFromDescriptor(descriptor), Candidate: candidate}
	if _, err := expectedUninstallUnit(lifecycle.config, transition); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("foreign unit error = %v", err)
	}
	if err := lifecycle.verifyDescriptorUnit(transition); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("foreign descriptor unit error = %v", err)
	}
	candidate.Arguments = []string{"bad\nargument"}
	transition.Transition.Candidate = candidate
	if _, err := expectedUninstallUnit(lifecycle.config, transition); err == nil {
		t.Fatal("invalid candidate unit rendered")
	}
}

func TestRemoveInactiveLifecycleSocketBranches(t *testing.T) {
	if staleSocketProbeTimeout != 100*time.Millisecond {
		t.Fatalf("stale socket probe timeout = %v", staleSocketProbeTimeout)
	}
	uid := uint32(os.Getuid())
	missing := filepath.Join(t.TempDir(), "missing.sock")
	if removed, err := removeInactiveLifecycleSocket(missing, uid); err != nil || removed {
		t.Fatalf("missing socket = %t, %v", removed, err)
	}
	filePath := filepath.Join(t.TempDir(), "file.sock")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeInactiveLifecycleSocket(filePath, uid); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("regular socket path error = %v", err)
	}
	activePath := filepath.Join(t.TempDir(), "active.sock")
	active, err := net.ListenUnix("unix", &net.UnixAddr{Name: activePath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if err := os.Chmod(activePath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeInactiveLifecycleSocket(activePath, uid); !errors.Is(err, ErrSocketActive) {
		t.Fatalf("active socket error = %v", err)
	}
	stalePath := filepath.Join(t.TempDir(), "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: stalePath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(stalePath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeInactiveLifecycleSocket(stalePath, uid); err != nil || !removed {
		t.Fatalf("stale socket = %t, %v", removed, err)
	}
	wrongModePath := filepath.Join(t.TempDir(), "mode.sock")
	wrongMode, err := net.ListenUnix("unix", &net.UnixAddr{Name: wrongModePath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer wrongMode.Close()
	if err := os.Chmod(wrongModePath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := removeInactiveLifecycleSocket(wrongModePath, uid); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("wrong-mode socket error = %v", err)
	}
}

func TestStageUpgradeRollsBackCopiedCandidateOnOversizedReceipt(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	upgrade.Arguments = []string{strings.Repeat("x", maxLifecycleDescriptor)}
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err == nil {
		t.Fatal("oversized staged receipt accepted")
	}
	candidate := filepath.Join(lifecycle.config.Paths.BinaryDirectory, upgrade.Version, "service")
	if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed staging retained candidate: %v", err)
	}
}

func TestStageUpgradeRejectsCorruptPreexistingCandidate(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	candidate := filepath.Join(lifecycle.config.Paths.BinaryDirectory, upgrade.Version, "service")
	if err := os.Mkdir(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("wrong"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("corrupt candidate error = %v", err)
	}
}

func TestIdempotentInstallReportsReloadFailure(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runner.setError(errors.New("reload failed"))
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("idempotent install error = %v", err)
	}
}

func TestActivateUpgradeAbortsAndResumesWhenPreviousRemainsActive(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	managed.ignoreStop = true
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if err == nil || result.RecoveryRequired {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	descriptor, _, readErr := lifecycle.readDescriptor()
	if readErr != nil || descriptor.Transition != nil {
		t.Fatalf("transition retained after safe abort: %+v, %v", descriptor.Transition, readErr)
	}
	assertManagedServiceReady(t, lifecycle)
}

func TestActivateUpgradeMarksRecoveryForInactiveNonterminalSource(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "deactivating", "stop-sigterm", "disabled", 42))
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
}

func TestActivateUpgradeRollsBackWhenReadyCandidateBecomesInactive(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	managed.nextVersion = upgrade.Version
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	shows := 0
	runner.setHook(func(executable string, arguments []string) error {
		if err := managed.command(executable, arguments); err != nil {
			return err
		}
		if len(arguments) > 1 && arguments[1] == "show" {
			shows++
			if shows == 3 {
				runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
			}
		}
		return nil
	})
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			managed.nextVersion = request.Version
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRolledBack) || !result.RolledBack || !result.Status.Active {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
}

func TestActivateUpgradeRejectsCandidateWithUnchangedPID(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	beforePID := managed.pid
	managed.nextVersion = upgrade.Version
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", beforePID))
	runner.setHook(func(executable string, arguments []string) error {
		if err := managed.command(executable, arguments); err != nil {
			return err
		}
		if len(arguments) > 1 && arguments[1] == "start" {
			runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", beforePID))
		}
		return nil
	})
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			managed.nextVersion = request.Version
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRolledBack) || !result.RolledBack {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
}

func TestActivateUpgradeStopsFailedPreCandidateRestore(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.setHook(func(executable string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "start" {
			managed.nextVersion = "wrong-previous-version"
		}
		err := managed.command(executable, arguments)
		if len(arguments) > 1 && arguments[1] == "stop" {
			cancel()
		}
		return err
	})
	result, err := lifecycle.ActivateUpgrade(ctx, UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired || managed.cancel != nil {
		t.Fatalf("activation result = %+v, %v running=%t", result, err, managed.cancel != nil)
	}
}

func TestActivateUpgradeMarksRecoveryWhenInterruptedStopIsNonterminal(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.setHook(func(executable string, arguments []string) error {
		err := managed.command(executable, arguments)
		if len(arguments) > 1 && arguments[1] == "stop" {
			runner.setStdout(lifecycleShowOutput(lifecycle, "deactivating", "stop-sigterm", "disabled", 42))
			cancel()
		}
		return err
	})
	result, err := lifecycle.ActivateUpgrade(ctx, UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
}

func TestActivateUpgradeMarksRecoveryWhenCandidateStopIsNonterminal(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	stopCalls := 0
	runner.setHook(func(executable string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "stop" {
			stopCalls++
			if stopCalls == 2 {
				return nil
			}
		}
		return managed.command(executable, arguments)
	})
	checkerCalls := 0
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			checkerCalls++
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired || checkerCalls != 0 || managed.cancel == nil {
		t.Fatalf("activation result = %+v, %v checker=%d running=%t", result, err, checkerCalls, managed.cancel != nil)
	}
}

func TestActivateUpgradePreservesCandidateStopCommandErrorThroughRollback(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	stopCalls := 0
	stopErr := errors.New("candidate stop command failed")
	runner.setHook(func(executable string, arguments []string) error {
		err := managed.command(executable, arguments)
		if len(arguments) > 1 && arguments[1] == "stop" {
			stopCalls++
			if stopCalls == 2 {
				return stopErr
			}
		}
		return err
	})
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			managed.nextVersion = request.Version
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRolledBack) || !errors.Is(err, stopErr) || !result.RolledBack {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
}

func TestRestartClosesClientWhenManagerObservationFails(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	runner.setHook(func(executable string, arguments []string) error {
		if err := managed.command(executable, arguments); err != nil {
			return err
		}
		if len(arguments) > 1 && arguments[1] == "restart" {
			runner.setError(errors.New("observation failed"))
		}
		return nil
	})
	if _, err := lifecycle.Restart(context.Background()); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("restart error = %v", err)
	}
}

func TestRestartRejectsFinalManagerLossAfterReadiness(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatal(err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	shows := 0
	runner.setHook(func(executable string, arguments []string) error {
		if err := managed.command(executable, arguments); err != nil {
			return err
		}
		if len(arguments) > 1 && arguments[1] == "show" {
			shows++
			if shows == 2 {
				runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
			}
		}
		return nil
	})
	status, err := lifecycle.Restart(context.Background())
	if err == nil || status.Active || status.MainPID != 0 || shows < 3 {
		t.Fatalf("restart result = %+v, %v shows=%d", status, err, shows)
	}
}

func TestUninstallRejectsNonterminalNotFoundManager(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installed.UnitPath); err != nil {
		t.Fatal(err)
	}
	runner.setStdout([]byte(fmt.Sprintf("MainPID=42\nResult=success\nId=%s\nLoadState=not-found\nActiveState=activating\nSubState=start\nFragmentPath=\nUnitFileState=disabled\n", lifecycle.config.UnitName)))
	if _, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true}); err == nil {
		t.Fatal("unsafe not-found manager state accepted")
	}
	if _, err := os.Lstat(installed.Executable); err != nil {
		t.Fatalf("uninstall removed executable: %v", err)
	}
}

func TestDescriptorForInstallRejectsEachSourceDimension(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Lifecycle, *InstallRequest)
	}{
		{"relative source", func(_ *Lifecycle, request *InstallRequest) { request.SourceExecutable = "service" }},
		{"invalid version", func(_ *Lifecycle, request *InstallRequest) { request.Version = "bad/version" }},
		{"control argument", func(_ *Lifecycle, request *InstallRequest) { request.Arguments = []string{"bad\narg"} }},
		{"source directory", func(_ *Lifecycle, request *InstallRequest) {
			request.SourceExecutable = filepath.Dir(request.SourceExecutable)
		}},
		{"source symlink", func(_ *Lifecycle, request *InstallRequest) {
			link := request.SourceExecutable + ".link"
			if err := os.Symlink(request.SourceExecutable, link); err != nil {
				panic(err)
			}
			request.SourceExecutable = link
		}},
		{"non executable", func(_ *Lifecycle, request *InstallRequest) {
			if err := os.Chmod(request.SourceExecutable, 0o600); err != nil {
				panic(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, _, request := lifecycleFixture(t)
			test.mutate(lifecycle, &request)
			if _, source, err := lifecycle.descriptorForInstall(request); err == nil {
				_ = source.Close()
				t.Fatal("invalid install source accepted")
			}
		})
	}
}

func TestReadDescriptorRejectsEachStoredDimension(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string)
	}{
		{"mode", func(path string) { _ = os.Chmod(path, 0o644) }},
		{"directory", func(path string) { _ = os.Remove(path); _ = os.Mkdir(path, 0o700) }},
		{"symlink", func(path string) {
			contents, _ := os.ReadFile(path)
			target := path + ".target"
			_ = os.WriteFile(target, contents, 0o600)
			_ = os.Remove(path)
			_ = os.Symlink(target, path)
		}},
		{"noncanonical", func(path string) {
			contents, _ := os.ReadFile(path)
			_ = os.WriteFile(path, append([]byte(" "), contents...), 0o600)
		}},
		{"unknown field", func(path string) {
			contents, _ := os.ReadFile(path)
			contents = bytes.Replace(contents, []byte("{\n"), []byte("{\n  \"unknown\": true,\n"), 1)
			_ = os.WriteFile(path, contents, 0o600)
		}},
		{"oversized", func(path string) { _ = os.WriteFile(path, bytes.Repeat([]byte("x"), maxLifecycleDescriptor+1), 0o600) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, _, request := lifecycleFixture(t)
			if _, err := lifecycle.Install(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			test.mutate(lifecycle.config.Paths.DescriptorPath)
			if _, exists, err := lifecycle.readDescriptor(); err == nil || exists {
				t.Fatalf("invalid descriptor read: exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestPurgeManifestPresenceMustMatchConfiguration(t *testing.T) {
	lifecycle, _, _ := lifecycleFixture(t)
	manifestPath := lifecycle.purgeManifestPath()
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.verifyPurgeManifest(nil); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("unexpected manifest error = %v", err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	config := lifecycle.config
	config.Paths.PurgePaths = []string{filepath.Join(filepath.Dir(config.Paths.BinaryDirectory), "state")}
	configured, err := NewLifecycle(config, lifecycle.runner)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := configured.purgeManifestBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.verifyPurgeManifest(expected); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("missing expected manifest error = %v", err)
	}
}

func TestUpgradeReceiptHelperFailureBranches(t *testing.T) {
	t.Run("nil transition phase", func(t *testing.T) {
		lifecycle, _, request := lifecycleFixture(t)
		if _, err := lifecycle.Install(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		descriptor, _, err := lifecycle.readDescriptor()
		if err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.setUpgradePhase(&descriptor, upgradeCandidateUnit); !errors.Is(err, ErrUpgradeRecoveryRequired) {
			t.Fatalf("nil transition phase error = %v", err)
		}
	})
	for _, operation := range []string{"mark", "abort"} {
		t.Run(operation, func(t *testing.T) {
			lifecycle, _, request := lifecycleFixture(t)
			if _, err := lifecycle.Install(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			upgrade := upgradeRequest(t, request, "2.0.0", "candidate")
			if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
				t.Fatal(err)
			}
			descriptor, _, err := lifecycle.readDescriptor()
			if err != nil {
				t.Fatal(err)
			}
			descriptor.Transition = &lifecycleTransition{
				Phase: upgradeStoppingPrevious, Previous: releaseFromDescriptor(descriptor), Candidate: *cloneLifecycleRelease(*descriptor.Pending),
			}
			if err := lifecycle.publishDescriptorTransition(descriptor); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lifecycle.config.Paths.DescriptorPath, 0o400); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("cause")
			if operation == "mark" {
				err = lifecycle.markUpgradeRecovery(descriptor, cause)
			} else {
				err = lifecycle.abortUpgradeBeforeReplacement(descriptor, nil, cause)
			}
			if !errors.Is(err, ErrUpgradeRecoveryRequired) || !errors.Is(err, cause) || !errors.Is(err, ErrLifecycleOwnership) {
				t.Fatalf("%s error = %v", operation, err)
			}
		})
	}
}

func TestControlServerProtocolAndCanceledResumeBranches(t *testing.T) {
	handler := &testMaintenanceHandler{resume: func(ctx context.Context) error { return ctx.Err() }}
	server, err := NewServer(ServerConfig{
		SocketPath: "/tmp/servicehost-control.sock", ProtocolVersion: 1,
		ServiceName: "service", ServiceVersion: "1", Maintenance: handler,
		InitialState: HealthReady, InitialAccepting: true,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	control := &controlServer{owner: server}
	if _, err := control.GetStatus(context.Background(), &servicehostv1.GetStatusRequest{ProtocolVersion: 2}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("GetStatus incompatible error = %v", err)
	}
	if _, err := control.Handshake(context.Background(), &servicehostv1.HandshakeRequest{ProtocolVersion: 1}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("Handshake invalid client error = %v", err)
	}
	before := server.Status()
	server.beforeDrain = &before
	server.drainCompleted = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := control.Resume(ctx, &servicehostv1.ResumeRequest{ProtocolVersion: 1}); grpcstatus.Code(err) != codes.Canceled {
		t.Fatalf("Resume canceled error = %v", err)
	}
}

func TestProtocolResponsesPreserveEveryStatusField(t *testing.T) {
	for _, state := range []HealthState{HealthStarting, HealthReady, HealthDraining, HealthDegraded, HealthStopping} {
		input := Status{Epoch: "epoch", Revision: 42, State: state, Accepting: true, Detail: "detail"}
		health := healthResponse(input)
		if health.GetEpoch() != input.Epoch || health.GetRevision() != input.Revision || publicHealth(health.GetState()) != state || !health.GetAccepting() || health.GetDetail() != input.Detail {
			t.Fatalf("health response for %d = %+v", state, health)
		}
		snapshot := statusSnapshot(input)
		if got := publicStatus(snapshot); got != input {
			t.Fatalf("status response for %d = %+v", state, got)
		}
	}
}

func TestReadinessPredicatesEveryDimension(t *testing.T) {
	handshake := Handshake{Cursor: Cursor{Epoch: "epoch", Revision: 5}}
	valid := Status{Epoch: "epoch", Revision: 5, State: HealthReady, Accepting: true}
	if !freshReadyStatus(valid, handshake) {
		t.Fatal("fresh ready status rejected")
	}
	for _, mutate := range []func(*Status){
		func(status *Status) { status.State = HealthStarting },
		func(status *Status) { status.Accepting = false },
		func(status *Status) { status.Epoch = "other" },
		func(status *Status) { status.Revision = 4 },
	} {
		candidate := valid
		mutate(&candidate)
		if freshReadyStatus(candidate, handshake) {
			t.Fatalf("non-ready status accepted: %+v", candidate)
		}
	}
	newer := valid
	newer.Revision = 6
	if !freshReadyStatus(newer, handshake) {
		t.Fatal("newer ready status rejected")
	}
	if !terminalReadinessError(ErrLifecycleOwnership) ||
		!terminalReadinessError(grpcstatus.Error(codes.FailedPrecondition, "version")) ||
		terminalReadinessError(grpcstatus.Error(codes.Unavailable, "retry")) || terminalReadinessError(nil) {
		t.Fatal("terminal readiness error classification changed")
	}
}

func TestClientDrainUsesDrainTimeout(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s")
	release := make(chan struct{})
	handler := &testMaintenanceHandler{drain: func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	_, stop := startTestServer(t, ServerConfig{
		SocketPath: socket, ProtocolVersion: 1, ServiceName: "service", ServiceVersion: "1",
		InitialState: HealthReady, InitialAccepting: true, Maintenance: handler,
	})
	defer stop()
	client, err := Dial(context.Background(), ClientConfig{
		SocketPath: socket, ProtocolVersion: 1, ClientName: "client",
		RPCTimeout: 5 * time.Millisecond, DrainTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	timer := time.NewTimer(25 * time.Millisecond)
	go func() {
		<-timer.C
		close(release)
	}()
	if _, err := client.Drain(context.Background()); err != nil {
		t.Fatalf("Drain used normal RPC timeout: %v", err)
	}
}

func TestNormalizeServeResultEveryOutcome(t *testing.T) {
	if normalizeServeResult(nil) != nil || normalizeServeResult(net.ErrClosed) != nil {
		t.Fatal("normal serve termination returned error")
	}
	cause := errors.New("serve failed")
	if err := normalizeServeResult(cause); !errors.Is(err, cause) {
		t.Fatalf("custom serve error = %v", err)
	}
}

func TestUnixPeerUIDRawConnectionFailures(t *testing.T) {
	if _, err := unixSocketPeerUID(fakeNetConn{}); err == nil {
		t.Fatal("connection without syscall access accepted")
	}
	if _, err := unixSocketPeerUID(fakeSyscallConn{syscallErr: errors.New("syscall failed")}); err == nil {
		t.Fatal("SyscallConn error accepted")
	}
	if _, err := unixSocketPeerUID(fakeSyscallConn{raw: fakeRawConn{controlErr: errors.New("control failed")}}); err == nil {
		t.Fatal("RawConn control error accepted")
	}
	if _, err := unixSocketPeerUID(fakeSyscallConn{raw: fakeRawConn{invokeControl: true}}); err == nil {
		t.Fatal("invalid socket fd accepted")
	}
	if _, err := readPeerCredential(fakeRawConn{controlErr: errors.New("control failed")}); err == nil {
		t.Fatal("direct control error accepted")
	}
	if _, err := readPeerCredential(fakeRawConn{invokeControl: true}); err == nil {
		t.Fatal("direct invalid socket fd accepted")
	}
}

type fakeNetConn struct{}

func (fakeNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeNetConn) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeNetConn) Close() error                     { return nil }
func (fakeNetConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "local", Net: "unix"} }
func (fakeNetConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "remote", Net: "unix"} }
func (fakeNetConn) SetDeadline(time.Time) error      { return nil }
func (fakeNetConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeNetConn) SetWriteDeadline(time.Time) error { return nil }

type fakeSyscallConn struct {
	fakeNetConn
	raw        syscall.RawConn
	syscallErr error
}

func (conn fakeSyscallConn) SyscallConn() (syscall.RawConn, error) { return conn.raw, conn.syscallErr }

type fakeRawConn struct {
	controlErr    error
	invokeControl bool
}

func (conn fakeRawConn) Control(callback func(uintptr)) error {
	if conn.invokeControl {
		callback(^uintptr(0))
	}
	return conn.controlErr
}
func (fakeRawConn) Read(func(uintptr) bool) error  { return nil }
func (fakeRawConn) Write(func(uintptr) bool) error { return nil }

func TestWatchStatusPropagatesInitialAndUpdateSendFailures(t *testing.T) {
	server, err := NewServer(ServerConfig{
		SocketPath: "/tmp/servicehost-watch.sock", ProtocolVersion: 1,
		ServiceName: "service", ServiceVersion: "1", InitialState: HealthReady, InitialAccepting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	control := &controlServer{owner: server}
	sendErr := errors.New("send failed")
	initialCtx, initialCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer initialCancel()
	initialStream := &failingWatchStream{ctx: initialCtx, err: sendErr}
	if err := control.WatchStatus(&servicehostv1.WatchStatusRequest{ProtocolVersion: 1}, initialStream); !errors.Is(err, sendErr) {
		t.Fatalf("initial send error = %v", err)
	}
	current := server.Status()
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer updateCancel()
	updateStream := &failingWatchStream{ctx: updateCtx, err: sendErr}
	done := make(chan error, 1)
	go func() {
		done <- control.WatchStatus(&servicehostv1.WatchStatusRequest{
			ProtocolVersion: 1, Epoch: current.Epoch, AfterRevision: current.Revision,
		}, updateStream)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		server.hub.mu.Lock()
		watchers := len(server.hub.watchers)
		server.hub.mu.Unlock()
		if watchers == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := server.UpdateStatus(HealthDegraded, false, "degraded"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, sendErr) {
		t.Fatalf("update send error = %v", err)
	}
}

type failingWatchStream struct {
	ctx context.Context
	err error
}

func (stream *failingWatchStream) Send(*servicehostv1.StatusUpdate) error { return stream.err }
func (*failingWatchStream) SetHeader(metadata.MD) error                   { return nil }
func (*failingWatchStream) SendHeader(metadata.MD) error                  { return nil }
func (*failingWatchStream) SetTrailer(metadata.MD)                        {}
func (stream *failingWatchStream) Context() context.Context               { return stream.ctx }
func (*failingWatchStream) SendMsg(any) error                             { return nil }
func (*failingWatchStream) RecvMsg(any) error                             { return io.EOF }

func TestPurgeEntryDisappearanceRemainsOwnershipError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	captured, err := inspectCreatedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	entry := lifecyclePurgeEntry{path: path, file: captured, mode: info.Mode()}
	if err := entry.remove(uint32(os.Getuid())); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("disappeared purge entry error = %v", err)
	}
}

func TestClientDefaultsAndDisconnectedStatusError(t *testing.T) {
	if err := closeLifecycleClient(nil); err != nil {
		t.Fatalf("close nil client: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "s")
	_, stop := startTestServer(t, ServerConfig{
		SocketPath: socket, ProtocolVersion: 1, ServiceName: "service", ServiceVersion: "1",
		InitialState: HealthReady, InitialAccepting: true,
	})
	defer stop()
	client, err := Dial(context.Background(), ClientConfig{SocketPath: socket, ProtocolVersion: 1, ClientName: "client"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if client.config.MaxMessageBytes != DefaultMaxMessageBytes || client.config.DialTimeout != 2*time.Second ||
		client.config.RPCTimeout != DefaultRPCTimeout || client.config.DrainTimeout != DefaultDrainTimeout {
		t.Fatalf("unexpected client defaults: %+v", client.config)
	}
	if err := closeLifecycleClient(client); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := client.Status(context.Background()); err == nil {
		t.Fatal("status on closed client succeeded")
	}
}
