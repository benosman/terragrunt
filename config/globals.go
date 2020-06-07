package config

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
)

// Detailed error messages in diagnostics returned by parsing globals
const (
	// A consistent error message for multiple globals block in terragrunt config (which is currently not supported)
	multipleGlobalsBlockDetail = "Terragrunt currently does not support multiple globals blocks in a single config. Consolidate to a single globals block."
)

// Global represents a single global name binding. This holds the unevaluated expression, extracted from the parsed file
// (but before decoding) so that we can look for references to other globals before evaluating.
type Global struct {
	Name string
	Expr hcl.Expression
}

// evaluateGlobalsBlock is a routine to evaluate the globals block in a way to allow references to other globals. This
// will:
// - Extract a reference to the globals block from the parsed file
// - Continuously evaluate the block until all references are evaluated, defering evaluation of anything that references
//   other globals until those references are evaluated.
// This returns a map of the global names to the evaluated expressions (represented as `cty.Value` objects). This will
// error if there are remaining unevaluated globals after all references that can be evaluated has been evaluated.
func evaluateGlobalsBlock(
	terragruntOptions *options.TerragruntOptions,
	parser *hclparse.Parser,
	hclFile *hcl.File,
	filename string,
	included *IncludeConfig,
) (map[string]cty.Value, error) {
	diagsWriter := util.GetDiagnosticsWriter(parser)

	globalsBlock, diags := getGlobalsBlock(hclFile)
	if diags.HasErrors() {
		diagsWriter.WriteDiagnostics(diags)
		return nil, errors.WithStackTrace(diags)
	}
	if globalsBlock == nil {
		// No globals block referenced in the file
		util.Debugf(terragruntOptions.Logger, "Did not find any globals block: skipping evaluation.")
		return nil, nil
	}

	util.Debugf(terragruntOptions.Logger, "Found globals block: evaluating the expressions.")

	globals, diags := decodeGlobalsBlock(globalsBlock)
	if diags.HasErrors() {
		terragruntOptions.Logger.Printf("Encountered error while decoding globals block into name expression pairs.")
		diagsWriter.WriteDiagnostics(diags)
		return nil, errors.WithStackTrace(diags)
	}

	// Continuously attempt to evaluate the globals until there are no more globals to evaluate, or we can't evaluate
	// further.
	evaluatedGlobals := map[string]cty.Value{}
	evaluated := true
	for iterations := 0; len(globals) > 0 && evaluated; iterations++ {
		if iterations > MaxIter {
			// Reached maximum supported iterations, which is most likely an infinite loop bug so cut the iteration
			// short an return an error.
			return nil, errors.WithStackTrace(MaxIterError{})
		}

		var err error
		globals, evaluatedGlobals, evaluated, err = attemptEvaluateGlobals(
			terragruntOptions,
			filename,
			globals,
			included,
			evaluatedGlobals,
			diagsWriter,
		)
		if err != nil {
			terragruntOptions.Logger.Printf("Encountered error while evaluating globals.")
			return nil, err
		}
	}
	if len(globals) > 0 {
		// This is an error because we couldn't evaluate all globals
		terragruntOptions.Logger.Printf("Not all globals could be evaluated:")
		for _, global := range globals {
			terragruntOptions.Logger.Printf("\t- %s", global.Name)
		}
		return nil, errors.WithStackTrace(CouldNotEvaluateAllGlobalsError{})
	}

	return evaluatedGlobals, nil
}

// attemptEvaluateGlobals attempts to evaluate the globals block given the map of already evaluated globals, replacing
// references to globals with the previously evaluated values. This will return:
// - the list of remaining globals that were unevaluated in this attempt
// - the updated map of evaluated globals after this attempt
// - whether or not any globals were evaluated in this attempt
// - any errors from the evaluation
func attemptEvaluateGlobals(
	terragruntOptions *options.TerragruntOptions,
	filename string,
	globals []*Global,
	included *IncludeConfig,
	evaluatedGlobals map[string]cty.Value,
	diagsWriter hcl.DiagnosticWriter,
) (unevaluatedGlobals []*Global, newEvaluatedGlobals map[string]cty.Value, evaluated bool, err error) {
	// The HCL2 parser and especially cty conversions will panic in many types of errors, so we have to recover from
	// those panics here and convert them to normal errors
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.WithStackTrace(
				PanicWhileParsingConfig{
					RecoveredValue: recovered,
					ConfigFile:     filename,
				},
			)
		}
	}()

	evaluatedGlobalsAsCty, err := convertValuesMapToCtyVal(evaluatedGlobals)
	if err != nil {
		terragruntOptions.Logger.Printf("Could not convert evaluated globals to the execution context to evaluate additional globals")
		return nil, evaluatedGlobals, false, err
	}
	evalCtx := CreateTerragruntEvalContext(
		filename,
		terragruntOptions,
		EvalContextExtensions{Include: included, Globals: &evaluatedGlobalsAsCty},
	)

	// Track the globals that were evaluated for logging purposes
	newlyEvaluatedGlobalNames := []string{}

	unevaluatedGlobals = []*Global{}
	evaluated = false
	newEvaluatedGlobals = map[string]cty.Value{}
	for key, val := range evaluatedGlobals {
		newEvaluatedGlobals[key] = val
	}
	for _, global := range globals {
		if canEvaluate(terragruntOptions, global.Expr, evaluatedGlobals) {
			evaluatedVal, diags := global.Expr.Value(evalCtx)
			if diags.HasErrors() {
				diagsWriter.WriteDiagnostics(diags)
				return nil, evaluatedGlobals, false, errors.WithStackTrace(diags)
			}
			newEvaluatedGlobals[global.Name] = evaluatedVal
			newlyEvaluatedGlobalNames = append(newlyEvaluatedGlobalNames, global.Name)
			evaluated = true
		} else {
			unevaluatedGlobals = append(unevaluatedGlobals, global)
		}
	}

	util.Debugf(
		terragruntOptions.Logger,
		"Evaluated %d globals (remaining %d): %s",
		len(newlyEvaluatedGlobalNames),
		len(unevaluatedGlobals),
		strings.Join(newlyEvaluatedGlobalNames, ", "),
	)
	return unevaluatedGlobals, newEvaluatedGlobals, evaluated, nil
}

// getGlobalName takes a variable reference encoded as a HCL tree traversal that is rooted at the name `global` and
// returns the underlying variable lookup on the global map. If it is not a global name lookup, this will return empty
// string.
func getGlobalName(terragruntOptions *options.TerragruntOptions, traversal hcl.Traversal) string {
	if traversal.IsRelative() {
		return ""
	}

	if traversal.RootName() != "global" {
		return ""
	}

	split := traversal.SimpleSplit()
	for _, relRaw := range split.Rel {
		switch rel := relRaw.(type) {
		case hcl.TraverseAttr:
			return rel.Name
		default:
			// This means that it is either an operation directly on the globals block, or is an unsupported action (e.g
			// a splat or lookup). Either way, there is no global name.
			continue
		}
	}
	return ""
}

// getGlobalsBlock takes a parsed HCL file and extracts a reference to the `globals` block, if there is one defined.
func getGlobalsBlock(hclFile *hcl.File) (*hcl.Block, hcl.Diagnostics) {
	globalsSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			hcl.BlockHeaderSchema{Type: "globals"},
		},
	}
	// We use PartialContent here, because we are only interested in parsing out the globals block.
	parsedGlobals, _, diags := hclFile.Body.PartialContent(globalsSchema)
	extractedGlobalsBlocks := []*hcl.Block{}
	for _, block := range parsedGlobals.Blocks {
		if block.Type == "globals" {
			extractedGlobalsBlocks = append(extractedGlobalsBlocks, block)
		}
	}
	// We currently only support parsing a single globals block
	if len(extractedGlobalsBlocks) == 1 {
		return extractedGlobalsBlocks[0], diags
	} else if len(extractedGlobalsBlocks) > 1 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Multiple globals block",
			Detail:   multipleGlobalsBlockDetail,
		})
		return nil, diags
	} else {
		// No globals block parsed
		return nil, diags
	}
}

// decodeGlobalsBlock loads the block into name expression pairs to assist with evaluation of the globals prior to
// evaluating the whole config. Note that this is exactly the same as
// terraform/configs/named_values.go:decodeGlobalsBlock
func decodeGlobalsBlock(globalsBlock *hcl.Block) ([]*Global, hcl.Diagnostics) {
	attrs, diags := globalsBlock.Body.JustAttributes()
	if len(attrs) == 0 {
		return nil, diags
	}

	globals := make([]*Global, 0, len(attrs))
	for name, attr := range attrs {
		if !hclsyntax.ValidIdentifier(name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid global value name",
				Detail:   badIdentifierDetail,
				Subject:  &attr.NameRange,
			})
		}

		globals = append(globals, &Global{
			Name: name,
			Expr: attr.Expr,
		})
	}
	return globals, diags
}

// ------------------------------------------------
// Custom Errors Returned by Functions in this Code
// ------------------------------------------------

type CouldNotEvaluateAllGlobalsError struct{}

func (err CouldNotEvaluateAllGlobalsError) Error() string {
	return "Could not evaluate all globals in block."
}