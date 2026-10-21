package main

import (
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

func TestArchGuardInjectedViolationsFail(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/internal/runtime/sample.go b/internal/runtime/sample.go",
		"+++ b/internal/runtime/sample.go",
		"+\tCoreState string `json:\"singbox_state\"`",
		"diff --git a/internal/commitlane/sample.go b/internal/commitlane/sample.go",
		"+++ b/internal/commitlane/sample.go",
		"+\t\"smartgate/internal/worker/xray\"",
		"diff --git a/internal/state/sample.go b/internal/state/sample.go",
		"+++ b/internal/state/sample.go",
		"+import \"smartgate/internal/singbox\"",
		"diff --git a/internal/provider/sample.go b/internal/provider/sample.go",
		"+++ b/internal/provider/sample.go",
		"+import(\"smartgate/internal/worker/singbox\")",
	}, "\n")
	failures := scanAddedDiff(diff)
	if len(failures) != 4 {
		t.Fatalf("injected violations were not rejected: %+v", failures)
	}
	classes := map[string]int{}
	for _, failure := range failures {
		classes[failure.Class]++
	}
	if classes["G2_JSON_KEY"] != 1 || classes["G6_FORBIDDEN_IMPORT"] != 3 {
		t.Fatalf("wrong injected violation classes: %+v", failures)
	}
}

func TestArchGuardAllowsOnlyDocumentedImplementationPaths(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/internal/worker/singbox/sample.go b/internal/worker/singbox/sample.go",
		"+++ b/internal/worker/singbox/sample.go",
		"+\tCoreState string `json:\"singbox_state\"`",
		"+\t\"smartgate/internal/singbox\"",
	}, "\n")
	if failures := scanAddedDiff(diff); len(failures) != 0 {
		t.Fatalf("documented implementation path was not exempt: %+v", failures)
	}
	nonGoDiff := strings.Join([]string{
		"diff --git a/scripts/sample.sh b/scripts/sample.sh",
		"+++ b/scripts/sample.sh",
		"+\"smartgate/internal/singbox\"",
	}, "\n")
	if failures := scanAddedDiff(nonGoDiff); len(failures) != 0 {
		t.Fatalf("non-Go text was interpreted as a Go import: %+v", failures)
	}
}

func TestArchGuardTestFixturesDoNotHideSharedLayerBranches(t *testing.T) {
	source := []byte(`package shared

func testCore(core string) bool {
	fixture := []string{"w_sb_1", "sing-box"}
	_ = fixture
	switch core {
	case "x" + "ray":
		return true
	}
	return false
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G5_FIXED_WORKER_ID", Package: "internal/shared"}]; got != 0 {
		t.Fatalf("fixture string counted as shared-layer worker branch: %d", got)
	}
	if got := counts[metricKey{Class: "G3_CORE_SWITCH", Package: "internal/shared"}]; got != 1 {
		t.Fatalf("shared-layer core switch escaped test scanning: %d", got)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}]; got == 0 {
		t.Fatal("shared-layer core branch string escaped test scanning")
	}
}

func TestArchGuardTestBranchStringsAreScanned(t *testing.T) {
	source := []byte(`package shared

func testCore(core, worker string) bool {
	return core == "sing-box" && worker == "w_sb_1"
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("test core branch string escaped scanning")
	}
	if counts[metricKey{Class: "G5_FIXED_WORKER_ID", Package: "internal/shared"}] == 0 {
		t.Fatal("test worker branch string escaped scanning")
	}
}

func TestArchGuardTestBranchIdentifiersCannotHideCoreLiterals(t *testing.T) {
	source := []byte(`package shared

var primary = "sing-box"

func route(core string) bool {
	return core == primary
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("indirect test branch core literal escaped token scanning")
	}
	if counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}] == 0 {
		t.Fatal("indirect test branch core literal escaped enum scanning")
	}
}

func TestArchGuardTestBranchHelpersCannotHideCoreLiterals(t *testing.T) {
	source := []byte(`package shared

func primary() string { return "sing-box" }

func route(core string) bool {
	return core == primary()
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("test helper return escaped token scanning")
	}
	if counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}] == 0 {
		t.Fatal("test helper return escaped enum scanning")
	}
}

func TestArchGuardTestBranchMethodSelectorsCannotHideCoreLiterals(t *testing.T) {
	source := []byte(`package shared

type fixture struct{}

func (fixture) primary() string { return "sing-box" }

func route(core string) bool {
	return core == (fixture{}).primary()
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("test method helper return escaped token scanning")
	}
	if counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}] == 0 {
		t.Fatal("test method helper return escaped enum scanning")
	}
}

func TestArchGuardCrossFileTestHelpersCannotHideCoreLiterals(t *testing.T) {
	sources := []goSource{
		{Path: "internal/shared/helper_test.go", Body: []byte(`package shared

func primary() string { return "xray" }
`)},
		{Path: "internal/shared/sample_test.go", Body: []byte(`package shared

func route(core string) bool {
	return core == primary()
}
`)},
	}
	counts := map[metricKey]int{}
	if err := scanGoFiles(sources, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("cross-file test helper return escaped token scanning")
	}
	if counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}] == 0 {
		t.Fatal("cross-file test helper return escaped enum scanning")
	}
}

func TestArchGuardTestBranchCompositeSourcesCannotHideCoreLiterals(t *testing.T) {
	source := []byte(`package shared

func route(core string) bool {
	fixture := struct{ Core string }{Core: "xray"}
	return core == fixture.Core
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("test composite source escaped token scanning")
	}
	if counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}] == 0 {
		t.Fatal("test composite source escaped enum scanning")
	}
}

func TestArchGuardTestBranchTracingRespectsLexicalObjects(t *testing.T) {
	source := []byte(`package shared

var expected = "sing-box"

type fixture struct{}

func (fixture) primary() string { return "xray" }

func route(got string) bool {
	{
		expected := "neutral"
		if got == expected {
			return true
		}
	}
	_ = expected
	return false
}

func methodRoute(got string) bool {
	type fixture struct {
		primary func() string
	}
	value := fixture{primary: func() string { return "neutral" }}
	return got == value.primary()
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}]; got != 0 {
		t.Fatalf("shadowed fixture was attached to the wrong lexical object: %d", got)
	}
	if got := counts[metricKey{Class: "G4_CORE_ENUM", Package: "internal/shared"}]; got != 0 {
		t.Fatalf("shadowed fixture produced a false enum finding: %d", got)
	}
}

func TestArchGuardTestConditionCallsAreScanned(t *testing.T) {
	source := []byte(`package shared

import "strings"

func testCore(value string) bool {
	if strings.Contains(value, "x" + "ray") {
		return true
	}
	return false
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample_test.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/shared"}] == 0 {
		t.Fatal("test condition call escaped core-token scanning")
	}
}

func TestArchGuardTestdataUsesDocumentedWhitelist(t *testing.T) {
	for _, path := range []string{
		"internal/fabric/testdata/m2probe/main.go",
		"scripts/testdata/tool.sh",
	} {
		if !isAllowedFile(path) {
			t.Fatalf("RFC testdata path %q was not exempt", path)
		}
	}
}

func TestArchGuardConstantConcatenationIsScanned(t *testing.T) {
	source := []byte(`package shared

const workerID = "w_" + "sb_1"

func route(core string) bool {
	switch core {
	case "x" + "ray":
		return true
	}
	return core == "sing" + "-box"
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"G1_CORE_TOKEN", "G3_CORE_SWITCH", "G4_CORE_ENUM", "G5_FIXED_WORKER_ID"} {
		if counts[metricKey{Class: class, Package: "internal/shared"}] == 0 {
			t.Fatalf("constant concatenation escaped %s scanning: %+v", class, counts)
		}
	}
}

func TestArchGuardContractDataAllowancesAreExactAndBounded(t *testing.T) {
	allowed := []byte(`package state

func legacyRestartServiceTaskType() string { return "restart_singbox" }
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/scheduler/scheduler.go", allowed, "internal/scheduler", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/scheduler"}]; got != 0 {
		t.Fatalf("required contract data was rejected: %d", got)
	}
	ledger := extractCompatibilityLedger(counts)
	if got := ledger[metricKey{Class: compatibilityMetricClass, Package: "internal/scheduler/scheduler.go#legacyRestartServiceTaskType"}]; got != 1 {
		t.Fatalf("required contract data was not ledgered: %d", got)
	}

	wrongSymbol := []byte(`package state

func anotherTaskType() string { return "restart_singbox" }
`)
	counts = map[metricKey]int{}
	if err := scanGoFile("internal/scheduler/scheduler.go", wrongSymbol, "internal/scheduler", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/scheduler"}]; got != 1 {
		t.Fatalf("same-file wrong-symbol contract data escaped: %d", got)
	}

	wrongShape := []byte(`package state

func legacyRestartServiceTaskType() string {
	value := "restart_singbox"
	return value
}
`)
	counts = map[metricKey]int{}
	if err := scanGoFile("internal/scheduler/scheduler.go", wrongShape, "internal/scheduler", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/scheduler"}]; got != 1 {
		t.Fatalf("wrong-shape contract data escaped: %d", got)
	}

	counts = map[metricKey]int{}
	if err := scanGoFile("internal/runtime/sample.go", allowed, "internal/runtime", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/runtime"}]; got != 1 {
		t.Fatalf("contract data allowance leaked to another file: %d", got)
	}
}

func TestArchGuardContractDataRequiresExactDeclarationShape(t *testing.T) {
	wrongFunctions := []string{
		`func legacyRestartServiceTaskType(_ ...int) string { return "restart_singbox" }`,
		`func legacyRestartServiceTaskType() any { return "restart_singbox" }`,
		`func legacyRestartServiceTaskType[T any]() string { return "restart_singbox" }`,
	}
	for _, declaration := range wrongFunctions {
		counts := map[metricKey]int{}
		source := []byte("package scheduler\n" + declaration + "\n")
		if err := scanGoFile("internal/scheduler/scheduler.go", source, "internal/scheduler", counts); err != nil {
			t.Fatal(err)
		}
		if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/scheduler"}]; got != 1 {
			t.Fatalf("wrong function shape escaped guard (%s): %d", declaration, got)
		}
	}

	counts := map[metricKey]int{}
	typedVariable := []byte(`package provider

var remoteProfileScheme any = "sing-box"
`)
	if err := scanGoFile("internal/provider/refresh.go", typedVariable, "internal/provider", counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[metricKey{Class: "G1_CORE_TOKEN", Package: "internal/provider"}]; got != 1 {
		t.Fatalf("typed compatibility variable escaped guard: %d", got)
	}
}

func TestArchGuardCompatibilityLedgerMustMatchRequiredBindings(t *testing.T) {
	complete := expectedCompatibilityLedger()
	if err := validateCompatibilityLedger(complete); err != nil {
		t.Fatalf("complete ledger rejected: %v", err)
	}

	missing := make(map[metricKey]int, len(complete))
	for key, count := range complete {
		missing[key] = count
	}
	for key := range missing {
		delete(missing, key)
		break
	}
	if err := validateCompatibilityLedger(missing); err == nil {
		t.Fatal("missing compatibility binding was accepted")
	}

	duplicate := make(map[metricKey]int, len(complete))
	for key, count := range complete {
		duplicate[key] = count
	}
	for key := range duplicate {
		duplicate[key]++
		break
	}
	if err := validateCompatibilityLedger(duplicate); err == nil {
		t.Fatal("duplicate compatibility binding was accepted")
	}

	extra := make(map[metricKey]int, len(complete)+1)
	for key, count := range complete {
		extra[key] = count
	}
	extra[metricKey{Class: compatibilityMetricClass, Package: "internal/runtime/example.go#wrong"}] = 1
	if err := validateCompatibilityLedger(extra); err == nil {
		t.Fatal("unexpected compatibility binding was accepted")
	}
}

func TestArchGuardRejectsCrossPackageCompatibilityReferenceReuse(t *testing.T) {
	sources := parseContractReferenceSources(t, map[string]string{
		"internal/state/spec.go": `package state

func SchedulerTaskRestartDefaultService() string { return "sing-box" }
`,
		"internal/runtime/sample.go": `package runtime

import "smartgate/internal/state"

func route(core string) bool {
	return core == state.SchedulerTaskRestartDefaultService()
}
`,
	})
	ledger := expectedCompatibilityLedger()
	if err := scanContractDataReferences(sources, ledger); err != nil {
		t.Fatal(err)
	}
	if err := validateCompatibilityLedger(ledger); err == nil {
		t.Fatal("cross-package compatibility helper reuse was accepted")
	}
}

func TestArchGuardRejectsSamePackageCompatibilityReferenceReuse(t *testing.T) {
	sources := parseContractReferenceSources(t, map[string]string{
		"internal/scheduler/scheduler.go": `package scheduler

func legacyRestartServiceTaskType() string { return "restart_singbox" }
`,
		"internal/scheduler/sample.go": `package scheduler

func route(task string) bool {
	switch task {
	case legacyRestartServiceTaskType():
		return true
	}
	return false
}
`,
	})
	ledger := expectedCompatibilityLedger()
	if err := scanContractDataReferences(sources, ledger); err != nil {
		t.Fatal(err)
	}
	if err := validateCompatibilityLedger(ledger); err == nil {
		t.Fatal("same-package compatibility helper reuse was accepted")
	}
}

func parseContractReferenceSources(t *testing.T, bodies map[string]string) []parsedGoSource {
	t.Helper()
	fileSet := token.NewFileSet()
	paths := make([]string, 0, len(bodies))
	for path := range bodies {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	sources := make([]parsedGoSource, 0, len(paths))
	for _, path := range paths {
		file, err := parser.ParseFile(fileSet, path, bodies[path], 0)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, parsedGoSource{Path: path, File: file})
	}
	return sources
}

func TestArchGuardTestFixtureJSONTagsAreAllowed(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/internal/runtime/sample_test.go b/internal/runtime/sample_test.go",
		"+++ b/internal/runtime/sample_test.go",
		"+\tState string `json:\"singbox_state\"`",
	}, "\n")
	if failures := scanAddedDiff(diff); len(failures) != 0 {
		t.Fatalf("test fixture schema was rejected: %+v", failures)
	}
}

func TestArchGuardRatchetRejectsIncreasesAndReportsDecreases(t *testing.T) {
	keyUp := metricKey{Class: "G1_CORE_TOKEN", Package: "internal/runtime"}
	keyDown := metricKey{Class: "G5_FIXED_WORKER_ID", Package: "internal/worker"}
	failures, decreases := compareBaseline(
		map[metricKey]int{keyUp: 2, keyDown: 3},
		map[metricKey]int{keyUp: 3, keyDown: 1},
	)
	if len(failures) != 1 || failures[0].Class != keyUp.Class || failures[0].Path != keyUp.Package {
		t.Fatalf("ratchet increase did not fail: %+v", failures)
	}
	if len(decreases) != 1 || !strings.Contains(decreases[0], keyDown.Package) {
		t.Fatalf("ratchet decrease was not reported: %+v", decreases)
	}
}

func TestArchGuardCommittedBaselineCannotIncrease(t *testing.T) {
	key := metricKey{Class: "G1_CORE_TOKEN", Package: "internal/runtime"}
	failures, _ := compareBaseline(
		map[metricKey]int{key: 2},
		map[metricKey]int{key: 3},
	)
	if len(failures) != 1 || failures[0].Class != key.Class || failures[0].Path != key.Package {
		t.Fatalf("raised baseline was not rejected: %+v", failures)
	}
}

func TestArchGuardInjectedEnumerationPatternsAreCounted(t *testing.T) {
	source := []byte(`package shared

const CoreSingBox = "sing-box"

func route(core string) string {
	switch core {
	case "xray":
		return "w_alpha_1"
	}
	return ""
}
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/shared/sample.go", source, "internal/shared", counts); err != nil {
		t.Fatal(err)
	}
	failures, _ := compareBaseline(map[metricKey]int{}, counts)
	failedClasses := map[string]bool{}
	for _, failure := range failures {
		failedClasses[failure.Class] = true
	}
	for _, class := range []string{"G1_CORE_TOKEN", "G3_CORE_SWITCH", "G4_CORE_ENUM", "G5_FIXED_WORKER_ID"} {
		if !failedClasses[class] {
			t.Fatalf("injected %s pattern did not fail the ratchet: %+v", class, failures)
		}
	}
}

func TestArchGuardInjectedFrontendPatternsAreCounted(t *testing.T) {
	counts := map[metricKey]int{}
	body := []byte(`
type Core = "sing-box" | "fakecore" | "xray"
const first = node.capability["xray"]
const second = node.capabilities.singBox
`)
	addMatches(counts, "G4_FRONTEND_CORE_UNION", "frontend/src", frontendUnionPattern, body)
	addMatches(counts, "FRONTEND_CAPABILITY_ACCESS", "frontend/src", frontendCapabilityPattern, body)
	failures, _ := compareBaseline(map[metricKey]int{}, counts)
	failedClasses := map[string]bool{}
	for _, failure := range failures {
		failedClasses[failure.Class] = true
	}
	for _, class := range []string{"G4_FRONTEND_CORE_UNION", "FRONTEND_CAPABILITY_ACCESS"} {
		if !failedClasses[class] {
			t.Fatalf("injected frontend %s pattern did not fail the ratchet: %+v", class, failures)
		}
	}
}

func TestArchGuardClashTokensUseDocumentedExemption(t *testing.T) {
	source := []byte(`package sample

const clashMode = "clash"
`)
	counts := map[metricKey]int{}
	if err := scanGoFile("internal/runtime/sample.go", source, "internal/runtime", counts); err != nil {
		t.Fatal(err)
	}
	if counts[metricKey{Class: "CLASH_TOKEN", Package: "internal/runtime"}] != 2 {
		t.Fatalf("shared-layer Clash tokens escaped scanning: %+v", counts)
	}

	allowed := map[metricKey]int{}
	if err := scanGoFile("internal/provider/clash_subscription.go", source, "internal/provider", allowed); err != nil {
		t.Fatal(err)
	}
	if allowed[metricKey{Class: "CLASH_TOKEN", Package: "internal/provider"}] != 0 {
		t.Fatalf("provider input-format implementation was not exempt: %+v", allowed)
	}
}

func TestArchGuardDependencyGraphCountsForbiddenPackages(t *testing.T) {
	dependencies := []string{
		"smartgate/internal/coredesc",
		"smartgate/internal/singbox",
		"smartgate/internal/worker/singbox",
		"smartgate/internal/worker/xray",
		"smartgate/internal/worker/fakecore",
	}
	if got := forbiddenDependencyCount(dependencies); got != 4 {
		t.Fatalf("forbidden dependency count = %d, want 4", got)
	}
}

func TestArchGuardAdapterPackageExemptionIsExact(t *testing.T) {
	for _, path := range []string{
		"internal/worker/singbox",
		"internal/worker/singbox/compiler",
		"internal/worker/xray",
		"internal/worker/fakecore",
	} {
		if !isAllowedPackage(path) {
			t.Fatalf("adapter package %q was not exempt", path)
		}
	}
	for _, path := range []string{
		"internal/worker/singbox_extra",
		"internal/worker/xraycopy",
		"internal/worker/fakecore2",
	} {
		if isAllowedPackage(path) {
			t.Fatalf("non-adapter package %q was exempt", path)
		}
	}
}

func TestArchGuardSelfExemptionIsExact(t *testing.T) {
	for _, path := range []string{
		"scripts/archguard",
		"scripts/archguard.go",
		"scripts/archguard_test.go",
		"scripts/archguard-baseline.tsv",
	} {
		if !isArchGuardFile(path) {
			t.Fatalf("guard artifact %q was not exempt", path)
		}
	}
	for _, path := range []string{"scripts/archguard-extra.go", "scripts/archguard.bypass.go"} {
		if isArchGuardFile(path) {
			t.Fatalf("unregistered guard-like path %q was exempt", path)
		}
	}
}
