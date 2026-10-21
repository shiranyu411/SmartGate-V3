package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const defaultBaselinePath = "scripts/archguard-baseline.tsv"

type metricKey struct {
	Class   string
	Package string
}

type violation struct {
	Class   string
	Path    string
	Message string
}

type contractDataBinding struct {
	Path     string
	Symbol   string
	Literal  string
	Variable bool
}

type contractDataReference struct {
	BindingPath   string
	BindingSymbol string
	Path          string
	Function      string
	Usage         string
	Count         int
}

const (
	compatibilityMetricClass          = "COMPATIBILITY_CONTRACT_DATA"
	compatibilityReferenceMetricClass = "COMPATIBILITY_CONTRACT_REFERENCE"
)

var (
	coreTokenPattern          = regexp.MustCompile(`(?i)sing-box|singbox|sing_box|xray|w_sb_|w_xr_|CoreSingBox|CoreXray`)
	clashTokenPattern         = regexp.MustCompile(`(?i)clash`)
	fixedWorkerPattern        = regexp.MustCompile(`\bw_(?:(?:sb|xr)_[a-z0-9_]*|[a-z]+_[0-9]+)\b`)
	frontendUnionPattern      = regexp.MustCompile(`(?is)(?:["'](?:sing-box|singbox|sing_box|xray)["']\s*\|\s*["'A-Za-z_$({\[]|["'A-Za-z0-9_$)}\]]\s*\|\s*["'](?:sing-box|singbox|sing_box|xray)["'])`)
	frontendCapabilityPattern = regexp.MustCompile(`(?is)capabilit(?:y|ies)\s*(?:(?:\?\.)?\[\s*["'](?:sing-box|singbox|sing_box|xray)["']\s*\]|(?:\?\.|\.)\s*(?:singbox|sing_box|xray)\b)`)
	jsonCoreKeyPattern        = regexp.MustCompile(`(?i)json:"[^"]*(?:sing-box|singbox|sing_box|xray)[^"]*"`)
	forbiddenImportPattern    = regexp.MustCompile(`^\s*(?:import\s*(?:\(\s*)?)?(?:[A-Za-z_.][A-Za-z0-9_.]*\s+)?"(smartgate/internal/(?:singbox|worker/(?:singbox|xray|fakecore)))"\s*\)?\s*(?://.*)?$`)
)

var forbiddenDependencies = map[string]struct{}{
	"smartgate/internal/singbox":         {},
	"smartgate/internal/worker/singbox":  {},
	"smartgate/internal/worker/xray":     {},
	"smartgate/internal/worker/fakecore": {},
}

var requiredContractDataBindings = []contractDataBinding{
	{Path: "internal/provider/refresh.go", Symbol: "remoteProfileScheme", Literal: "sing-box", Variable: true},
	{Path: "internal/scheduler/scheduler.go", Symbol: "legacyRestartServiceTaskType", Literal: "restart_singbox"},
	{Path: "internal/state/data_plane.go", Symbol: "legacyDataPlaneRunningField", Literal: "sing_box_running"},
	{Path: "internal/state/spec.go", Symbol: "SchedulerTaskRestartDefaultService", Literal: "sing-box"},
	{Path: "internal/state/spec.go", Symbol: "legacyRestartServiceTaskType", Literal: "restart_singbox"},
}

var requiredContractDataReferences = []contractDataReference{
	{BindingPath: "internal/provider/refresh.go", BindingSymbol: "remoteProfileScheme", Path: "internal/provider/refresh.go", Function: "subscriptionUserAgent", Usage: "ident/binary(+).left", Count: 2},
	{BindingPath: "internal/provider/refresh.go", BindingSymbol: "remoteProfileScheme", Path: "internal/provider/subscription_url.go", Function: "EffectiveSubscriptionURL", Usage: "ident/binary(==).right"},
	{BindingPath: "internal/scheduler/scheduler.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/scheduler/scheduler.go", Function: "NormalizeTaskType", Usage: "call/case"},
	{BindingPath: "internal/scheduler/scheduler.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/scheduler/scheduler_test.go", Function: "TestNormalizeTaskTypeAliases", Usage: "call/binary(==).right"},
	{BindingPath: "internal/scheduler/scheduler.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/scheduler/scheduler_test.go", Function: "TestNormalizeTaskTypeAliases", Usage: "call/call.arg"},
	{BindingPath: "internal/scheduler/scheduler.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/scheduler/scheduler_test.go", Function: "TestRunNowLegacyRestartAliasUsesRestarter", Usage: "call/call.arg"},
	{BindingPath: "internal/scheduler/scheduler.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/scheduler/scheduler_test.go", Function: "TestRunNowLegacyRestartAliasWithoutRestarterNotRunnable", Usage: "call/call.arg"},
	{BindingPath: "internal/state/data_plane.go", BindingSymbol: "legacyDataPlaneRunningField", Path: "internal/state/data_plane.go", Function: "UnmarshalJSON", Usage: "call/index"},
	{BindingPath: "internal/state/data_plane.go", BindingSymbol: "legacyDataPlaneRunningField", Path: "internal/state/store_test.go", Function: "TestSaveDataPlaneObservationWritesNeutralGatewayPlane", Usage: "call/index"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/nodes_handler_test.go", Function: "<package>", Usage: "call/keyvalue", Count: 2},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_daemon_test.go", Function: "TestSchedulerDaemonRunsDueServiceRestart", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_handler_test.go", Function: "TestSchedulerRunTaskByIDLegacyServiceRestartAlias", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_handler_test.go", Function: "TestSchedulerRunTaskByIDLegacyServiceRestartAlias", Usage: "call/binary(!=).right"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_handler_test.go", Function: "TestSchedulerRunTaskByIDRestartsConfiguredService", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_handler_test.go", Function: "TestSchedulerRunTaskByIDServiceRestartFailureReturnsBadGateway", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/runtime/scheduler_handler_test.go", Function: "TestSchedulerServiceRestartRestoresDurableNonDefaultSelection", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/scheduler/scheduler_test.go", Function: "TestRunNowLegacyRestartAliasUsesRestarter", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/state/spec.go", Function: "DefaultSpecState", Usage: "call/keyvalue"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/state/spec.go", Function: "NormalizeSchedulerTaskSpec", Usage: "call/assign"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/state/store_test.go", Function: "TestDefaultSpecUsesParameterizedServiceRestartTask", Usage: "call/binary(!=).right"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "SchedulerTaskRestartDefaultService", Path: "internal/state/store_test.go", Function: "TestLoadSpecNormalizesLegacySchedulerAliases", Usage: "call/binary(!=).right"},
	{BindingPath: "internal/state/spec.go", BindingSymbol: "legacyRestartServiceTaskType", Path: "internal/state/spec.go", Function: "NormalizeSchedulerTaskSpec", Usage: "call/case"},
}

func main() {
	root := flag.String("root", "", "repository root")
	baseline := flag.String("baseline", defaultBaselinePath, "baseline path relative to the repository root")
	base := flag.String("base", os.Getenv("ARCHGUARD_BASE"), "git revision used for hard-failure addition checks")
	writeBaseline := flag.Bool("write-baseline", false, "replace the ratchet baseline with current counts")
	flag.Parse()

	if *root == "" {
		resolved, err := commandOutput("", "git", "rev-parse", "--show-toplevel")
		if err != nil {
			fatal(err)
		}
		*root = strings.TrimSpace(resolved)
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}
	if *base == "" {
		*base = "HEAD"
	}

	counts, err := scanRepository(absRoot)
	if err != nil {
		fatal(err)
	}
	compatibilityLedger := extractCompatibilityLedger(counts)
	if err := validateCompatibilityLedger(compatibilityLedger); err != nil {
		fatal(err)
	}
	for _, entry := range formatCompatibilityLedger(compatibilityLedger) {
		fmt.Println(entry)
	}

	baselinePath := *baseline
	if !filepath.IsAbs(baselinePath) {
		baselinePath = filepath.Join(absRoot, baselinePath)
	}
	baselineRepoPath, err := repositoryRelativePath(absRoot, baselinePath)
	if err != nil {
		fatal(err)
	}
	baseBaseline, baseHasBaseline, err := readBaselineAtRevision(absRoot, *base, baselineRepoPath)
	if err != nil {
		fatal(err)
	}
	hardFailureBase := *base
	if !baseHasBaseline {
		introduction, found, findErr := findBaselineIntroduction(absRoot, baselineRepoPath)
		if findErr != nil {
			fatal(findErr)
		}
		if found {
			baseBaseline, baseHasBaseline, err = readBaselineAtRevision(absRoot, introduction, baselineRepoPath)
			if err != nil {
				fatal(err)
			}
			if !baseHasBaseline {
				fatal(fmt.Errorf("baseline introduction %s does not contain %s", introduction, baselineRepoPath))
			}
			hardFailureBase = introduction
			fmt.Printf("archguard: base has no guard; using introduction %s as bootstrap boundary\n", introduction)
		} else {
			hardFailureBase = "HEAD"
			fmt.Println("archguard: base has no guard; using HEAD as uncommitted bootstrap boundary")
		}
	}
	hardFailures, err := scanAddedHardFailures(absRoot, hardFailureBase)
	if err != nil {
		fatal(err)
	}
	if len(hardFailures) > 0 {
		fatal(formatViolations(hardFailures))
	}
	if *writeBaseline {
		if baseHasBaseline {
			failures, _ := compareBaseline(baseBaseline, counts)
			if len(failures) > 0 {
				fatal(fmt.Errorf("refusing to raise committed baseline\n%w", formatViolations(failures)))
			}
		}
		if err := writeBaselineFile(baselinePath, counts); err != nil {
			fatal(err)
		}
		fmt.Printf("archguard: wrote %d ratchet entries to %s\n", len(counts), filepath.ToSlash(*baseline))
		return
	}

	want, err := readBaselineFile(baselinePath)
	if err != nil {
		fatal(err)
	}
	if baseHasBaseline {
		baselineFailures, baselineDecreases := compareBaseline(baseBaseline, want)
		for _, decrease := range baselineDecreases {
			fmt.Println(strings.Replace(decrease, "baseline can decrease", "baseline decreased", 1))
		}
		if len(baselineFailures) > 0 {
			for index := range baselineFailures {
				baselineFailures[index].Class = "BASELINE_" + baselineFailures[index].Class
			}
			fatal(formatViolations(baselineFailures))
		}
	}
	ratchetFailures, decreases := compareBaseline(want, counts)
	for _, decrease := range decreases {
		fmt.Println(decrease)
	}
	if len(ratchetFailures) > 0 {
		fatal(formatViolations(ratchetFailures))
	}
	fmt.Printf("archguard: passed (%d current ratchet entries; G2/G6 additions clean)\n", len(counts))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "archguard:", err)
	os.Exit(1)
}

func scanRepository(root string) (map[metricKey]int, error) {
	counts := map[metricKey]int{}
	files, err := sourceFiles(root)
	if err != nil {
		return nil, err
	}
	fileSet := token.NewFileSet()
	goPackages := map[string][]parsedGoSource{}
	var allGoSources []parsedGoSource
	for _, path := range files {
		if isAllowedFile(path) || isArchGuardFile(path) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		if filepath.Ext(path) == ".go" {
			parsed, err := parser.ParseFile(fileSet, path, body, 0)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			allGoSources = append(allGoSources, parsedGoSource{Path: path, File: parsed})
			key := packageKey(path) + "\x00" + parsed.Name.Name
			goPackages[key] = append(goPackages[key], parsedGoSource{Path: path, File: parsed})
			continue
		}
		if err := scanSourceFile(path, body, counts); err != nil {
			return nil, err
		}
	}
	if err := scanContractDataReferences(allGoSources, counts); err != nil {
		return nil, err
	}
	packageKeys := make([]string, 0, len(goPackages))
	for key := range goPackages {
		packageKeys = append(packageKeys, key)
	}
	sort.Strings(packageKeys)
	for _, key := range packageKeys {
		sources := goPackages[key]
		if err := scanParsedGoPackage(sources, packageKey(sources[0].Path), counts); err != nil {
			return nil, err
		}
	}
	if err := scanDependencyGraph(root, counts); err != nil {
		return nil, err
	}
	return counts, nil
}

func scanSourceFile(path string, body []byte, counts map[metricKey]int) error {
	pkg := packageKey(path)
	if filepath.Ext(path) == ".go" {
		if err := scanGoFile(path, body, pkg, counts); err != nil {
			return err
		}
	} else {
		addMatches(counts, "G1_CORE_TOKEN", pkg, coreTokenPattern, body)
		addMatches(counts, "CLASH_TOKEN", pkg, clashTokenPattern, body)
		addMatches(counts, "G5_FIXED_WORKER_ID", pkg, fixedWorkerPattern, body)
	}
	if isFrontendSource(path) {
		addMatches(counts, "G4_FRONTEND_CORE_UNION", pkg, frontendUnionPattern, body)
		addMatches(counts, "FRONTEND_CAPABILITY_ACCESS", pkg, frontendCapabilityPattern, body)
	}
	return nil
}

func scanGoFile(path string, body []byte, pkg string, counts map[metricKey]int) error {
	return scanGoFiles([]goSource{{Path: path, Body: body}}, pkg, counts)
}

type goSource struct {
	Path string
	Body []byte
}

type parsedGoSource struct {
	Path string
	File *ast.File
}

func scanGoFiles(sources []goSource, pkg string, counts map[metricKey]int) error {
	fileSet := token.NewFileSet()
	parsed := make([]parsedGoSource, 0, len(sources))
	packageName := ""
	for _, source := range sources {
		file, err := parser.ParseFile(fileSet, source.Path, source.Body, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", source.Path, err)
		}
		if packageName == "" {
			packageName = file.Name.Name
		} else if file.Name.Name != packageName {
			return fmt.Errorf("scan Go files: mixed packages %s and %s", packageName, file.Name.Name)
		}
		parsed = append(parsed, parsedGoSource{Path: source.Path, File: file})
	}
	return scanParsedGoPackage(parsed, pkg, counts)
}

func scanParsedGoPackage(sources []parsedGoSource, pkg string, counts map[metricKey]int) error {
	fixtureStrings := map[token.Pos]struct{}{}
	for _, source := range sources {
		for position := range fixtureStringPositions(source.File, strings.HasSuffix(source.Path, "_test.go")) {
			fixtureStrings[position] = struct{}{}
		}
	}
	branchFixturePositions, branchCoreExpressions := indirectTestBranchFixtures(sources)
	for position := range branchFixturePositions {
		delete(fixtureStrings, position)
	}
	for _, source := range sources {
		scanParsedGoFile(source.Path, source.File, pkg, fixtureStrings, branchCoreExpressions, counts)
	}
	return nil
}

func scanParsedGoFile(path string, parsed *ast.File, pkg string, fixtureStrings, branchCoreExpressions map[token.Pos]struct{}, counts map[metricKey]int) {
	clashAllowed := isClashAllowedFile(path)
	contractDataPositions := boundContractDataPositions(parsed, filepath.ToSlash(path))
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			addStringMatches(counts, "G1_CORE_TOKEN", pkg, coreTokenPattern, value.Name)
			if !clashAllowed {
				addStringMatches(counts, "CLASH_TOKEN", pkg, clashTokenPattern, value.Name)
			}
		case *ast.BasicLit:
			if value.Kind != token.STRING {
				return true
			}
			if _, fixture := fixtureStrings[value.Pos()]; fixture {
				return true
			}
			literal, unquoteErr := strconv.Unquote(value.Value)
			if unquoteErr != nil {
				return true
			}
			if symbol, compatible := contractDataPositions[value.Pos()]; compatible {
				counts[metricKey{Class: compatibilityMetricClass, Package: filepath.ToSlash(path) + "#" + symbol}]++
			} else {
				addStringMatches(counts, "G1_CORE_TOKEN", pkg, coreTokenPattern, literal)
			}
			if !clashAllowed {
				addStringMatches(counts, "CLASH_TOKEN", pkg, clashTokenPattern, literal)
			}
			addStringMatches(counts, "G5_FIXED_WORKER_ID", pkg, fixedWorkerPattern, literal)
		case *ast.BinaryExpr:
			literal, ok := constantString(value)
			if !ok || expressionIsFixture(value, fixtureStrings) {
				return true
			}
			addStringMatches(counts, "G1_CORE_TOKEN", pkg, coreTokenPattern, literal)
			if !clashAllowed {
				addStringMatches(counts, "CLASH_TOKEN", pkg, clashTokenPattern, literal)
			}
			addStringMatches(counts, "G5_FIXED_WORKER_ID", pkg, fixedWorkerPattern, literal)
		}
		return true
	})

	ast.Inspect(parsed, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CaseClause:
			for _, expression := range value.List {
				_, indirectIdentity := branchCoreExpressions[expression.Pos()]
				if containsCoreIdentity(expression) || indirectIdentity {
					counts[metricKey{Class: "G3_CORE_SWITCH", Package: pkg}]++
				}
			}
		case *ast.GenDecl:
			if value.Tok != token.CONST {
				return true
			}
			for _, spec := range value.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if ok && valueSpecContainsCoreIdentity(valueSpec) {
					counts[metricKey{Class: "G4_CORE_ENUM", Package: pkg}]++
				}
			}
		case *ast.BinaryExpr:
			_, indirectIdentity := branchCoreExpressions[value.Pos()]
			if (value.Op == token.EQL || value.Op == token.NEQ) &&
				(containsCoreIdentity(value.X) || containsCoreIdentity(value.Y) || indirectIdentity) {
				counts[metricKey{Class: "G4_CORE_ENUM", Package: pkg}]++
			}
		}
		return true
	})
}

func boundContractDataPositions(file *ast.File, path string) map[token.Pos]string {
	positions := map[token.Pos]string{}
	for _, binding := range requiredContractDataBindings {
		if binding.Path != path {
			continue
		}
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				if binding.Variable || value.Name.Name != binding.Symbol || !exactStringFunction(value) {
					continue
				}
				result, ok := value.Body.List[0].(*ast.ReturnStmt)
				if !ok || len(result.Results) != 1 {
					continue
				}
				literal, ok := result.Results[0].(*ast.BasicLit)
				if ok && stringLiteralEquals(literal, binding.Literal) {
					positions[literal.Pos()] = binding.Symbol
				}
			case *ast.GenDecl:
				if !binding.Variable || value.Tok != token.VAR {
					continue
				}
				for _, item := range value.Specs {
					spec, ok := item.(*ast.ValueSpec)
					if !ok || spec.Type != nil || len(spec.Names) != 1 || len(spec.Values) != 1 || spec.Names[0].Name != binding.Symbol {
						continue
					}
					literal, ok := spec.Values[0].(*ast.BasicLit)
					if ok && stringLiteralEquals(literal, binding.Literal) {
						positions[literal.Pos()] = binding.Symbol
					}
				}
			}
		}
	}
	return positions
}

func exactStringFunction(function *ast.FuncDecl) bool {
	if function == nil || function.Recv != nil || function.Type == nil || function.Type.TypeParams != nil || function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	if function.Type.Params != nil && len(function.Type.Params.List) != 0 {
		return false
	}
	if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
		return false
	}
	result := function.Type.Results.List[0]
	resultType, ok := result.Type.(*ast.Ident)
	return ok && len(result.Names) == 0 && resultType.Name == "string"
}

func stringLiteralEquals(literal *ast.BasicLit, want string) bool {
	if literal == nil || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == want
}

type resolvedContractBinding struct {
	spec           contractDataBinding
	packageKey     string
	packageName    string
	importPath     string
	declaration    *ast.Object
	declarationPos token.Pos
}

func scanContractDataReferences(sources []parsedGoSource, counts map[metricKey]int) error {
	bindings := resolveContractBindings(sources)
	for _, source := range sources {
		imports, dotImports := contractImportAliases(source.File, bindings)
		stack := []ast.Node{}
		ast.Inspect(source.File, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			binding, reference := contractReference(node, stack, source, bindings, imports, dotImports)
			if binding != nil {
				parent := ast.Node(nil)
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				key := contractReferenceKey(*binding, source.Path, enclosingFunction(stack), immediateReferenceUsage(reference, parent))
				counts[key]++
			}
			stack = append(stack, node)
			return true
		})
	}
	return nil
}

func resolveContractBindings(sources []parsedGoSource) []*resolvedContractBinding {
	var resolved []*resolvedContractBinding
	for _, binding := range requiredContractDataBindings {
		for _, source := range sources {
			if source.Path != binding.Path {
				continue
			}
			object, position := contractDeclaration(source.File, binding)
			resolved = append(resolved, &resolvedContractBinding{
				spec: binding, packageKey: packageKey(source.Path) + "\x00" + source.File.Name.Name,
				packageName: source.File.Name.Name, importPath: "smartgate/" + packageKey(source.Path),
				declaration: object, declarationPos: position,
			})
			break
		}
	}
	return resolved
}

func contractDeclaration(file *ast.File, binding contractDataBinding) (*ast.Object, token.Pos) {
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *ast.FuncDecl:
			if !binding.Variable && value.Name.Name == binding.Symbol {
				return value.Name.Obj, value.Name.Pos()
			}
		case *ast.GenDecl:
			if !binding.Variable {
				continue
			}
			for _, item := range value.Specs {
				spec, ok := item.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range spec.Names {
					if name.Name == binding.Symbol {
						return name.Obj, name.Pos()
					}
				}
			}
		}
	}
	return nil, token.NoPos
}

func contractImportAliases(file *ast.File, bindings []*resolvedContractBinding) (map[string]string, map[string]bool) {
	aliases := map[string]string{}
	dotImports := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		for _, binding := range bindings {
			if binding.importPath != importPath {
				continue
			}
			if spec.Name == nil {
				aliases[binding.packageName] = importPath
			} else if spec.Name.Name == "." {
				dotImports[importPath] = true
			} else if spec.Name.Name != "_" {
				aliases[spec.Name.Name] = importPath
			}
			break
		}
	}
	return aliases, dotImports
}

func contractReference(node ast.Node, stack []ast.Node, source parsedGoSource, bindings []*resolvedContractBinding, imports map[string]string, dotImports map[string]bool) (*resolvedContractBinding, ast.Node) {
	parent := ast.Node(nil)
	if len(stack) > 0 {
		parent = stack[len(stack)-1]
	}
	switch value := node.(type) {
	case *ast.CallExpr:
		return resolveContractExpression(value.Fun, source, bindings, imports, dotImports), value
	case *ast.SelectorExpr:
		if call, ok := parent.(*ast.CallExpr); ok && call.Fun == value {
			return nil, nil
		}
		return resolveContractExpression(value, source, bindings, imports, dotImports), value
	case *ast.Ident:
		if selector, ok := parent.(*ast.SelectorExpr); ok && (selector.X == value || selector.Sel == value) {
			return nil, nil
		}
		if call, ok := parent.(*ast.CallExpr); ok && call.Fun == value {
			return nil, nil
		}
		binding := resolveContractExpression(value, source, bindings, imports, dotImports)
		if binding != nil && value.Pos() != binding.declarationPos {
			return binding, value
		}
	}
	return nil, nil
}

func resolveContractExpression(expression ast.Expr, source parsedGoSource, bindings []*resolvedContractBinding, imports map[string]string, dotImports map[string]bool) *resolvedContractBinding {
	switch value := expression.(type) {
	case *ast.Ident:
		packageID := packageKey(source.Path) + "\x00" + source.File.Name.Name
		for _, binding := range bindings {
			if binding.packageKey == packageID && binding.spec.Symbol == value.Name {
				if value.Obj == nil || value.Obj == binding.declaration {
					return binding
				}
				return nil
			}
			if value.Obj == nil && dotImports[binding.importPath] && binding.spec.Symbol == value.Name {
				return binding
			}
		}
	case *ast.SelectorExpr:
		qualifier, ok := value.X.(*ast.Ident)
		if !ok || qualifier.Obj != nil {
			return nil
		}
		importPath := imports[qualifier.Name]
		for _, binding := range bindings {
			if binding.importPath == importPath && binding.spec.Symbol == value.Sel.Name {
				return binding
			}
		}
	}
	return nil
}

func enclosingFunction(stack []ast.Node) string {
	for index := len(stack) - 1; index >= 0; index-- {
		if function, ok := stack[index].(*ast.FuncDecl); ok {
			return function.Name.Name
		}
	}
	return "<package>"
}

func immediateReferenceUsage(reference, parent ast.Node) string {
	kind := "ident"
	if _, ok := reference.(*ast.CallExpr); ok {
		kind = "call"
	} else if _, ok := reference.(*ast.SelectorExpr); ok {
		kind = "selector"
	}
	switch value := parent.(type) {
	case *ast.BinaryExpr:
		side := "right"
		if value.X == reference {
			side = "left"
		}
		return kind + "/binary(" + value.Op.String() + ")." + side
	case *ast.CallExpr:
		return kind + "/call.arg"
	case *ast.CaseClause:
		return kind + "/case"
	case *ast.IndexExpr:
		return kind + "/index"
	case *ast.AssignStmt:
		return kind + "/assign"
	case *ast.KeyValueExpr:
		return kind + "/keyvalue"
	case *ast.ReturnStmt:
		return kind + "/return"
	default:
		return fmt.Sprintf("%s/%T", kind, parent)
	}
}

func contractReferenceKey(binding resolvedContractBinding, path, function, usage string) metricKey {
	return metricKey{
		Class:   compatibilityReferenceMetricClass,
		Package: fmt.Sprintf("%s#%s -> %s#%s [%s]", binding.spec.Path, binding.spec.Symbol, filepath.ToSlash(path), function, usage),
	}
}

func extractCompatibilityLedger(counts map[metricKey]int) map[metricKey]int {
	ledger := map[metricKey]int{}
	for key, count := range counts {
		if key.Class != compatibilityMetricClass && key.Class != compatibilityReferenceMetricClass {
			continue
		}
		ledger[key] = count
		delete(counts, key)
	}
	return ledger
}

func validateCompatibilityLedger(ledger map[metricKey]int) error {
	expected := expectedCompatibilityLedger()
	var failures []string
	for key, want := range expected {
		if ledger[key] != want {
			failures = append(failures, fmt.Sprintf("compatibility binding %s count=%d, want %d", key.Package, ledger[key], want))
		}
	}
	for key, count := range ledger {
		if _, ok := expected[key]; !ok {
			failures = append(failures, fmt.Sprintf("unexpected compatibility binding %s count=%d", key.Package, count))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	return fmt.Errorf("%s", strings.Join(failures, "\n"))
}

func expectedCompatibilityLedger() map[metricKey]int {
	expected := make(map[metricKey]int, len(requiredContractDataBindings)+len(requiredContractDataReferences))
	for _, binding := range requiredContractDataBindings {
		key := metricKey{Class: compatibilityMetricClass, Package: binding.Path + "#" + binding.Symbol}
		expected[key]++
	}
	for _, reference := range requiredContractDataReferences {
		var binding *resolvedContractBinding
		for _, item := range requiredContractDataBindings {
			if item.Path == reference.BindingPath && item.Symbol == reference.BindingSymbol {
				binding = &resolvedContractBinding{spec: item}
				break
			}
		}
		if binding != nil {
			key := contractReferenceKey(*binding, reference.Path, reference.Function, reference.Usage)
			count := reference.Count
			if count == 0 {
				count = 1
			}
			expected[key] += count
		}
	}
	return expected
}

func formatCompatibilityLedger(ledger map[metricKey]int) []string {
	keys := make(map[metricKey]struct{}, len(ledger))
	for key := range ledger {
		keys[key] = struct{}{}
	}
	lines := make([]string, 0, len(keys))
	for _, key := range sortedMetricKeys(keys) {
		lines = append(lines, fmt.Sprintf("archguard: compatibility ledger: %s %d", key.Package, ledger[key]))
	}
	return lines
}

func fixtureStringPositions(file *ast.File, enabled bool) map[token.Pos]struct{} {
	positions := map[token.Pos]struct{}{}
	if !enabled {
		return positions
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			positions[literal.Pos()] = struct{}{}
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		var expressions []ast.Expr
		switch value := node.(type) {
		case *ast.CaseClause:
			expressions = value.List
		case *ast.IfStmt:
			expressions = []ast.Expr{value.Cond}
		case *ast.ForStmt:
			if value.Cond != nil {
				expressions = []ast.Expr{value.Cond}
			}
		case *ast.BinaryExpr:
			if value.Op == token.EQL || value.Op == token.NEQ {
				expressions = []ast.Expr{value.X, value.Y}
			}
		case *ast.IndexExpr:
			expressions = []ast.Expr{value.Index}
		}
		for _, expression := range expressions {
			ast.Inspect(expression, func(child ast.Node) bool {
				literal, ok := child.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					delete(positions, literal.Pos())
				}
				return true
			})
		}
		return true
	})
	return positions
}

func indirectTestBranchFixtures(sources []parsedGoSource) (map[token.Pos]struct{}, map[token.Pos]struct{}) {
	positions := map[token.Pos]struct{}{}
	coreExpressions := map[token.Pos]struct{}{}
	tracer := newTestBranchTracer(sources, positions)
	for _, source := range sources {
		if !strings.HasSuffix(source.Path, "_test.go") {
			continue
		}
		ast.Inspect(source.File, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CaseClause:
				for _, expression := range value.List {
					if tracer.containsCore(expression, map[*ast.Object]bool{}, true) {
						coreExpressions[expression.Pos()] = struct{}{}
					}
				}
			case *ast.IfStmt:
				tracer.containsCore(value.Cond, map[*ast.Object]bool{}, true)
			case *ast.ForStmt:
				if value.Cond != nil {
					tracer.containsCore(value.Cond, map[*ast.Object]bool{}, true)
				}
			case *ast.BinaryExpr:
				if value.Op == token.EQL || value.Op == token.NEQ {
					if tracer.containsCore(value.X, map[*ast.Object]bool{}, true) || tracer.containsCore(value.Y, map[*ast.Object]bool{}, true) {
						coreExpressions[value.Pos()] = struct{}{}
					}
				}
			case *ast.IndexExpr:
				tracer.containsCore(value.Index, map[*ast.Object]bool{}, true)
			}
			return true
		})
	}
	return positions, coreExpressions
}

func assignedExpressions(index, targets int, values []ast.Expr) []ast.Expr {
	if len(values) == targets && index < len(values) {
		return values[index : index+1]
	}
	return values
}

type testBranchTracer struct {
	definitions   map[*ast.Object][]ast.Expr
	explicitTypes map[*ast.Object]*ast.Object
	packageNames  map[string]*ast.Object
	types         map[string]*ast.Object
	methods       map[*ast.Object]map[string]*ast.Object
	positions     map[token.Pos]struct{}
}

func newTestBranchTracer(sources []parsedGoSource, positions map[token.Pos]struct{}) *testBranchTracer {
	tracer := &testBranchTracer{
		definitions:   map[*ast.Object][]ast.Expr{},
		explicitTypes: map[*ast.Object]*ast.Object{},
		packageNames:  map[string]*ast.Object{},
		types:         map[string]*ast.Object{},
		methods:       map[*ast.Object]map[string]*ast.Object{},
		positions:     positions,
	}
	for _, source := range sources {
		file := source.File
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.GenDecl:
				for _, item := range value.Specs {
					spec, ok := item.(*ast.TypeSpec)
					if ok {
						object := declarationObject(spec.Name, ast.Typ, spec)
						tracer.types[spec.Name.Name] = object
						tracer.packageNames[spec.Name.Name] = object
					}
				}
			case *ast.FuncDecl:
				if value.Recv == nil && strings.HasSuffix(source.Path, "_test.go") {
					tracer.packageNames[value.Name.Name] = declarationObject(value.Name, ast.Fun, value)
				}
			}
		}
	}
	for _, source := range sources {
		if !strings.HasSuffix(source.Path, "_test.go") {
			continue
		}
		file := source.File
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			receiver := tracer.typeObject(function.Recv.List[0].Type)
			if receiver == nil {
				continue
			}
			if tracer.methods[receiver] == nil {
				tracer.methods[receiver] = map[string]*ast.Object{}
			}
			tracer.methods[receiver][function.Name.Name] = declarationObject(function.Name, ast.Fun, function)
		}
	}
	for _, source := range sources {
		file := source.File
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.ValueSpec:
				for index, name := range value.Names {
					object := tracer.resolveObject(name)
					if object == nil {
						continue
					}
					tracer.definitions[object] = append(tracer.definitions[object], assignedExpressions(index, len(value.Names), value.Values)...)
					if objectType := tracer.typeObject(value.Type); objectType != nil {
						tracer.explicitTypes[object] = objectType
					}
				}
			case *ast.AssignStmt:
				for index, target := range value.Lhs {
					name, ok := target.(*ast.Ident)
					if !ok {
						continue
					}
					if object := tracer.resolveObject(name); object != nil {
						tracer.definitions[object] = append(tracer.definitions[object], assignedExpressions(index, len(value.Lhs), value.Rhs)...)
					}
				}
			case *ast.FuncDecl:
				tracer.recordFieldTypes(value.Recv)
				tracer.recordFieldTypes(value.Type.Params)
			}
			return true
		})
	}
	return tracer
}

func declarationObject(name *ast.Ident, kind ast.ObjKind, declaration any) *ast.Object {
	if name.Obj != nil {
		return name.Obj
	}
	return &ast.Object{Kind: kind, Name: name.Name, Decl: declaration}
}

func (tracer *testBranchTracer) recordFieldTypes(fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		objectType := tracer.typeObject(field.Type)
		if objectType == nil {
			continue
		}
		for _, name := range field.Names {
			if name.Obj != nil {
				tracer.explicitTypes[name.Obj] = objectType
			}
		}
	}
}

func (tracer *testBranchTracer) resolveObject(identifier *ast.Ident) *ast.Object {
	if identifier == nil {
		return nil
	}
	if identifier.Obj != nil {
		return identifier.Obj
	}
	return tracer.packageNames[identifier.Name]
}

func (tracer *testBranchTracer) typeObject(expression ast.Expr) *ast.Object {
	switch value := expression.(type) {
	case *ast.Ident:
		if value.Obj != nil && value.Obj.Kind == ast.Typ {
			return value.Obj
		}
		return tracer.types[value.Name]
	case *ast.StarExpr:
		return tracer.typeObject(value.X)
	case *ast.IndexExpr:
		return tracer.typeObject(value.X)
	case *ast.IndexListExpr:
		return tracer.typeObject(value.X)
	case *ast.ParenExpr:
		return tracer.typeObject(value.X)
	default:
		return nil
	}
}

func (tracer *testBranchTracer) containsCore(expression ast.Expr, visiting map[*ast.Object]bool, followCallArgs bool) bool {
	if expression == nil {
		return false
	}
	if literal, ok := constantString(expression); ok && isCoreLiteral(literal) {
		tracer.markStringLiterals(expression)
		return true
	}
	switch value := expression.(type) {
	case *ast.Ident:
		if isCoreIdentifier(value.Name) {
			return true
		}
		return tracer.objectContainsCore(tracer.resolveObject(value), visiting)
	case *ast.BinaryExpr:
		return tracer.containsCore(value.X, visiting, followCallArgs) || tracer.containsCore(value.Y, visiting, followCallArgs)
	case *ast.ParenExpr:
		return tracer.containsCore(value.X, visiting, followCallArgs)
	case *ast.UnaryExpr:
		return tracer.containsCore(value.X, visiting, followCallArgs)
	case *ast.CallExpr:
		if identifier, ok := value.Fun.(*ast.Ident); ok {
			if identifier.Name == "string" && len(value.Args) == 1 {
				return tracer.containsCore(value.Args[0], visiting, followCallArgs)
			}
			if tracer.objectContainsCore(tracer.resolveObject(identifier), visiting) {
				return true
			}
		}
		if selector, ok := value.Fun.(*ast.SelectorExpr); ok {
			for _, receiver := range tracer.receiverTypes(selector.X, map[*ast.Object]bool{}) {
				if tracer.objectContainsCore(tracer.methods[receiver][selector.Sel.Name], visiting) {
					return true
				}
			}
		}
		if followCallArgs {
			for _, argument := range value.Args {
				if tracer.containsCore(argument, visiting, true) {
					return true
				}
			}
		}
	case *ast.SelectorExpr:
		return tracer.containsCore(value.X, visiting, followCallArgs)
	case *ast.IndexExpr:
		return tracer.containsCore(value.X, visiting, followCallArgs) || tracer.containsCore(value.Index, visiting, followCallArgs)
	case *ast.IndexListExpr:
		if tracer.containsCore(value.X, visiting, followCallArgs) {
			return true
		}
		for _, index := range value.Indices {
			if tracer.containsCore(index, visiting, followCallArgs) {
				return true
			}
		}
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			expression, ok := element.(ast.Expr)
			if ok && tracer.containsCore(expression, visiting, followCallArgs) {
				return true
			}
		}
	case *ast.KeyValueExpr:
		return tracer.containsCore(value.Key, visiting, followCallArgs) || tracer.containsCore(value.Value, visiting, followCallArgs)
	}
	return false
}

func (tracer *testBranchTracer) receiverTypes(expression ast.Expr, visiting map[*ast.Object]bool) []*ast.Object {
	seen := map[*ast.Object]struct{}{}
	var collect func(ast.Expr)
	collect = func(current ast.Expr) {
		switch value := current.(type) {
		case *ast.CompositeLit:
			if objectType := tracer.typeObject(value.Type); objectType != nil {
				seen[objectType] = struct{}{}
			}
		case *ast.Ident:
			object := tracer.resolveObject(value)
			if object == nil || visiting[object] {
				return
			}
			if object.Kind == ast.Typ {
				seen[object] = struct{}{}
				return
			}
			if objectType := tracer.explicitTypes[object]; objectType != nil {
				seen[objectType] = struct{}{}
			}
			visiting[object] = true
			for _, definition := range tracer.definitions[object] {
				collect(definition)
			}
			delete(visiting, object)
		case *ast.ParenExpr:
			collect(value.X)
		case *ast.UnaryExpr:
			collect(value.X)
		case *ast.StarExpr:
			collect(value.X)
		case *ast.IndexExpr:
			collect(value.X)
		case *ast.IndexListExpr:
			collect(value.X)
		case *ast.CallExpr:
			identifier, ok := value.Fun.(*ast.Ident)
			if !ok {
				return
			}
			if identifier.Name == "new" && len(value.Args) == 1 {
				if objectType := tracer.typeObject(value.Args[0]); objectType != nil {
					seen[objectType] = struct{}{}
				}
				return
			}
			if objectType := tracer.typeObject(identifier); objectType != nil {
				seen[objectType] = struct{}{}
			}
		}
	}
	collect(expression)
	out := make([]*ast.Object, 0, len(seen))
	for object := range seen {
		out = append(out, object)
	}
	return out
}

func (tracer *testBranchTracer) objectContainsCore(object *ast.Object, visiting map[*ast.Object]bool) bool {
	if object == nil || visiting[object] {
		return false
	}
	visiting[object] = true
	defer delete(visiting, object)
	for _, expression := range tracer.definitions[object] {
		if tracer.containsCore(expression, visiting, false) {
			return true
		}
	}
	function, ok := object.Decl.(*ast.FuncDecl)
	if !ok || function.Body == nil {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if found {
			return false
		}
		result, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, expression := range result.Results {
			if tracer.containsCore(expression, visiting, true) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func (tracer *testBranchTracer) markStringLiterals(expression ast.Expr) {
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			tracer.positions[literal.Pos()] = struct{}{}
		}
		return true
	})
}

func expressionIsFixture(node ast.Node, fixtureStrings map[token.Pos]struct{}) bool {
	found := false
	fixture := true
	ast.Inspect(node, func(child ast.Node) bool {
		literal, ok := child.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		found = true
		if _, ok := fixtureStrings[literal.Pos()]; !ok {
			fixture = false
		}
		return true
	})
	return found && fixture
}

func constantString(expression ast.Expr) (string, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		literal, err := strconv.Unquote(value.Value)
		return literal, err == nil
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, leftOK := constantString(value.X)
		right, rightOK := constantString(value.Y)
		return left + right, leftOK && rightOK
	case *ast.ParenExpr:
		return constantString(value.X)
	default:
		return "", false
	}
}

func valueSpecContainsCoreIdentity(spec *ast.ValueSpec) bool {
	for _, name := range spec.Names {
		if isCoreIdentifier(name.Name) {
			return true
		}
	}
	for _, value := range spec.Values {
		if containsCoreIdentity(value) {
			return true
		}
	}
	return false
}

func containsCoreIdentity(node ast.Node) bool {
	if expression, ok := node.(ast.Expr); ok {
		if literal, constant := constantString(expression); constant && isCoreLiteral(literal) {
			return true
		}
	}
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		switch value := child.(type) {
		case *ast.Ident:
			if isCoreIdentifier(value.Name) {
				found = true
				return false
			}
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				literal, err := strconv.Unquote(value.Value)
				if err == nil && isCoreLiteral(literal) {
					found = true
					return false
				}
			}
		}
		return !found
	})
	return found
}

func isCoreLiteral(value string) bool {
	switch strings.ToLower(value) {
	case "sing-box", "singbox", "sing_box", "xray":
		return true
	default:
		return false
	}
}

func isCoreIdentifier(value string) bool {
	return value == "CoreSingBox" || value == "CoreXray"
}

func addMatches(counts map[metricKey]int, class, pkg string, pattern *regexp.Regexp, body []byte) {
	if count := len(pattern.FindAllIndex(body, -1)); count > 0 {
		counts[metricKey{Class: class, Package: pkg}] += count
	}
}

func addStringMatches(counts map[metricKey]int, class, pkg string, pattern *regexp.Regexp, value string) {
	if count := len(pattern.FindAllStringIndex(value, -1)); count > 0 {
		counts[metricKey{Class: class, Package: pkg}] += count
	}
}

func scanDependencyGraph(root string, counts map[metricKey]int) error {
	output, err := commandOutput(root, "go", "list", "-f", "{{.ImportPath}}", "./...")
	if err != nil {
		return err
	}
	for _, importPath := range nonEmptyLines(output) {
		relative := strings.TrimPrefix(importPath, "smartgate/")
		if relative == importPath || isAllowedPackage(relative) {
			continue
		}
		dependencies, err := commandOutput(root, "go", "list", "-deps", "-f", "{{.ImportPath}}", importPath)
		if err != nil {
			return err
		}
		if count := forbiddenDependencyCount(nonEmptyLines(dependencies)); count > 0 {
			counts[metricKey{Class: "G6_FORBIDDEN_DEP", Package: relative}] += count
		}
	}
	return nil
}

func forbiddenDependencyCount(dependencies []string) int {
	count := 0
	for _, dependency := range dependencies {
		if _, forbidden := forbiddenDependencies[dependency]; forbidden {
			count++
		}
	}
	return count
}

func scanAddedHardFailures(root, base string) ([]violation, error) {
	diff, err := commandOutput(root, "git", "diff", "--no-ext-diff", "--unified=0", base, "--", ".")
	if err != nil {
		return nil, err
	}
	return scanAddedDiff(diff), nil
}

func scanAddedDiff(diff string) []violation {
	var failures []violation
	currentPath := ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			headerPath := strings.TrimPrefix(line, "+++ ")
			if headerPath == "/dev/null" {
				currentPath = ""
			} else {
				currentPath = strings.TrimPrefix(headerPath, "b/")
			}
			continue
		}
		if currentPath == "" || !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		if isAllowedFile(currentPath) || isArchGuardFile(currentPath) {
			continue
		}
		if filepath.Ext(currentPath) != ".go" {
			continue
		}
		addition := strings.TrimPrefix(line, "+")
		if !strings.HasSuffix(currentPath, "_test.go") && jsonCoreKeyPattern.MatchString(addition) {
			failures = append(failures, violation{Class: "G2_JSON_KEY", Path: currentPath, Message: strings.TrimSpace(addition)})
		}
		if match := forbiddenImportPattern.FindStringSubmatch(addition); len(match) == 2 {
			failures = append(failures, violation{Class: "G6_FORBIDDEN_IMPORT", Path: currentPath, Message: match[1]})
		}
	}
	return failures
}

func compareBaseline(baseline, current map[metricKey]int) ([]violation, []string) {
	keys := make(map[metricKey]struct{}, len(baseline)+len(current))
	for key := range baseline {
		keys[key] = struct{}{}
	}
	for key := range current {
		keys[key] = struct{}{}
	}
	ordered := sortedMetricKeys(keys)
	var failures []violation
	var decreases []string
	for _, key := range ordered {
		want := baseline[key]
		got := current[key]
		switch {
		case got > want:
			failures = append(failures, violation{
				Class:   key.Class,
				Path:    key.Package,
				Message: fmt.Sprintf("count increased from %d to %d", want, got),
			})
		case got < want:
			decreases = append(decreases, fmt.Sprintf("archguard: baseline can decrease: %s %s %d -> %d", key.Class, key.Package, want, got))
		}
	}
	return failures, decreases
}

func readBaselineFile(path string) (map[metricKey]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseBaseline(path, file)
}

func readBaselineAtRevision(root, revision, path string) (map[metricKey]int, bool, error) {
	tracked, err := commandOutput(root, "git", "ls-tree", "--name-only", revision, "--", path)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(tracked) == "" {
		return nil, false, nil
	}
	contents, err := commandOutput(root, "git", "show", revision+":"+path)
	if err != nil {
		return nil, false, err
	}
	result, err := parseBaseline(revision+":"+path, strings.NewReader(contents))
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func findBaselineIntroduction(root, path string) (string, bool, error) {
	output, err := commandOutput(root, "git", "log", "--diff-filter=A", "--format=%H", "--reverse", "HEAD", "--", path)
	if err != nil {
		return "", false, err
	}
	revisions := nonEmptyLines(output)
	if len(revisions) == 0 {
		return "", false, nil
	}
	return revisions[0], true, nil
}

func parseBaseline(source string, reader io.Reader) (map[metricKey]int, error) {
	result := map[metricKey]int{}
	scanner := bufio.NewScanner(reader)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: expected class, package, count", source, lineNumber)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil || count < 0 {
			return nil, fmt.Errorf("%s:%d: invalid count %q", source, lineNumber, fields[2])
		}
		key := metricKey{Class: fields[0], Package: fields[1]}
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("%s:%d: duplicate metric %s/%s", source, lineNumber, key.Class, key.Package)
		}
		result[key] = count
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func repositoryRelativePath(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("baseline must be inside repository: %s", path)
	}
	return filepath.ToSlash(relative), nil
}

func writeBaselineFile(path string, counts map[metricKey]int) error {
	keys := make(map[metricKey]struct{}, len(counts))
	for key, count := range counts {
		if count > 0 {
			keys[key] = struct{}{}
		}
	}
	var builder strings.Builder
	builder.WriteString("# SmartGate architecture guard ratchet v1\n")
	builder.WriteString("# class<TAB>package<TAB>count\n")
	for _, key := range sortedMetricKeys(keys) {
		fmt.Fprintf(&builder, "%s\t%s\t%d\n", key.Class, key.Package, counts[key])
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func sortedMetricKeys(keys map[metricKey]struct{}) []metricKey {
	ordered := make([]metricKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Class != ordered[right].Class {
			return ordered[left].Class < ordered[right].Class
		}
		return ordered[left].Package < ordered[right].Package
	})
	return ordered
}

func sourceFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && excludedDirectory(relative) {
				return filepath.SkipDir
			}
			return nil
		}
		if supportedSource(relative) {
			files = append(files, relative)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func excludedDirectory(path string) bool {
	first := strings.Split(path, "/")[0]
	switch first {
	case ".git", ".gocache", ".tmp", ".smoke", ".devvm", "dist":
		return true
	}
	return path == "frontend/node_modules" || path == "frontend/dist" || path == "docs/archive"
}

func supportedSource(path string) bool {
	if filepath.Base(path) == "Makefile" {
		return true
	}
	switch filepath.Ext(path) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".sh":
		return true
	}
	return (strings.HasPrefix(path, "scripts/") || strings.HasPrefix(path, "configs/")) && filepath.Ext(path) == ""
}

func isAllowedFile(path string) bool {
	path = filepath.ToSlash(path)
	for _, prefix := range []string{
		"internal/worker/singbox/",
		"internal/worker/xray/",
		"internal/worker/fakecore/",
		"internal/singbox/",
		"docs/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if strings.Contains("/"+path+"/", "/testdata/") {
		return true
	}
	switch path {
	case "cmd/smartgate/main.go",
		"scripts/smartgate-worker-control",
		"frontend/src/lib/errorText.ts",
		"frontend/src/lib/statusText.ts",
		"frontend/src/lib/coreLabels.ts":
		return true
	}
	return false
}

func isArchGuardFile(path string) bool {
	switch path {
	case "scripts/archguard",
		"scripts/archguard.go",
		"scripts/archguard_test.go",
		"scripts/archguard-baseline.tsv":
		return true
	default:
		return false
	}
}

func isAllowedPackage(path string) bool {
	if path == "cmd/smartgate" || path == "internal/singbox" {
		return true
	}
	for _, adapter := range []string{
		"internal/worker/singbox",
		"internal/worker/xray",
		"internal/worker/fakecore",
	} {
		if path == adapter || strings.HasPrefix(path, adapter+"/") {
			return true
		}
	}
	return false
}

func isFrontendSource(path string) bool {
	return strings.HasPrefix(path, "frontend/src/") && (filepath.Ext(path) == ".ts" || filepath.Ext(path) == ".tsx")
}

func isClashAllowedFile(path string) bool {
	return path == "internal/provider/clash_subscription.go" || path == "internal/provider/clash_subscription_test.go"
}

func packageKey(path string) string {
	directory := filepath.ToSlash(filepath.Dir(path))
	if directory == "" {
		return "."
	}
	return directory
}

func commandOutput(directory, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func formatViolations(violations []violation) error {
	sort.Slice(violations, func(left, right int) bool {
		if violations[left].Class != violations[right].Class {
			return violations[left].Class < violations[right].Class
		}
		if violations[left].Path != violations[right].Path {
			return violations[left].Path < violations[right].Path
		}
		return violations[left].Message < violations[right].Message
	})
	var builder strings.Builder
	builder.WriteString("architecture guard failed")
	for _, item := range violations {
		fmt.Fprintf(&builder, "\n%s %s: %s", item.Class, item.Path, item.Message)
	}
	return fmt.Errorf("%s", builder.String())
}
