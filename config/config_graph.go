package config

import (
	"fmt"
	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/terraform/dag"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

const local = "local"
const global = "global"
const include = "include"

type rootVertex struct {
}

type variableVertex struct {
	Evaluator *configEvaluator
	Type   string
	Name   string
	Expr   hcl.Expression
	Evaluated bool
}

// basicEdge is a basic implementation of Edge that has the source and
// target vertex.
type basicEdge struct {
	S, T dag.Vertex
}

func (e *basicEdge) Hashcode() interface{} {
	return fmt.Sprintf("%p-%p", e.S, e.T)
}

func (e *basicEdge) Source() dag.Vertex {
	return e.S
}

func (e *basicEdge) Target() dag.Vertex {
	return e.T
}

type evaluatorGlobals struct {
	options  *options.TerragruntOptions
	parser   *hclparse.Parser
	graph    dag.AcyclicGraph
	root     rootVertex
	vertices map[string]variableVertex
	values   map[string]cty.Value
}

type configEvaluator struct {
	globals evaluatorGlobals

	configPath   string
	configFile   *hcl.File

	localVertices  map[string]variableVertex
	localValues    map[string]cty.Value
	includeVertex  *variableVertex
}

type EvaluationResult struct {
	ConfigFile *hcl.File
	Variables  map[string]cty.Value
}

func newConfigEvaluator(configFile *hcl.File, configPath string, globals evaluatorGlobals) *configEvaluator {
	eval := configEvaluator{}
	eval.globals = globals
	eval.configPath = configPath
	eval.configFile = configFile

	eval.localVertices = map[string]variableVertex{}
	eval.localValues = map[string]cty.Value{}

	eval.includeVertex = nil

	return &eval
}

// Evaluation Steps:
// 1. Parse child HCL, extract locals, globals
// 2. Add vertices for child locals, globals
// 3. Add edges for child variables based on interpolations used
//     a. When encountering globals that aren't defined in this config, create a vertex for them with an empty expression
// 4. Verify DAG and reduce graph
// 5. Evaluate everything except globals
// 6. If include exists, find parent HCL, parse, and extract locals and globals
// 7. Add vertices for parent locals
// 8. Add vertices for parent globals that don't already exist, or add expressions to empty globals
// 9. Verify and reduce graph
//     a. Verify that there are no globals that are empty.
// 10. Evaluate everything, skipping things that were evaluated in (5)
func ParseConfigVariables(
	terragruntOptions *options.TerragruntOptions,
	preparse *TerragruntPreparseResult,
) (*EvaluationResult, *EvaluationResult, error) {
	globals := evaluatorGlobals{
		options:  terragruntOptions,
		parser:   preparse.Parser,
		graph:    dag.AcyclicGraph{},
		root:     rootVertex{},
		vertices: map[string]variableVertex{},
		values:   map[string]cty.Value{},
	}

	// Add root of graph
	globals.graph.Add(globals.root)

	child := *newConfigEvaluator(preparse.File, preparse.Filename, globals)
	var childResult *EvaluationResult = nil

	// 1, 2, 3, 4
	err := child.decodeConfig()
	if err != nil {
		return nil, nil, err
	}

	// 5
	diags := globals.evaluateVariables(false)
	if diags != nil {
		return nil, nil, diags
	}

	var parent *configEvaluator = nil
	var parentResult *EvaluationResult = nil

	if preparse.Parent != nil {
		// 6, 7, 8, 9
		parent = newConfigEvaluator(preparse.Parent.File, preparse.Parent.Filename, globals)
		err = (*parent).decodeConfig()
		if err != nil {
			return nil, nil, err
		}
	}

	// 10
	diags = globals.evaluateVariables(true)
	if diags != nil {
		return nil, nil, diags
	}

	childResult, err = child.toResult()
	if err != nil {
		return nil, nil, err
	}
	if parent != nil {
		parentResult, err = parent.toResult()
		if err != nil {
			return nil, nil, err
		}
	}

	return childResult, parentResult, nil
}

func (eval *configEvaluator) decodeConfig() error {
	localsBlock, globalsBlock, diags := eval.getBlocks(eval.configFile)
	if diags != nil && diags.HasErrors() {
		return diags
	}

	var addedVertices []variableVertex

	if localsBlock != nil {
		err := eval.addVertices(local, localsBlock.Body, func(vertex variableVertex) error {
			eval.localVertices[vertex.Name] = vertex
			addedVertices = append(addedVertices, vertex)
			return nil
		})
		if err != nil {
			return err
		}
	}

	if globalsBlock != nil {
		err := eval.addVertices(global, globalsBlock.Body, func(vertex variableVertex) error {
			eval.globals.vertices[vertex.Name] = vertex
			addedVertices = append(addedVertices, vertex)
			return nil
		})
		if err != nil {
			return err
		}
	}

	err := eval.addAllEdges(eval.localVertices)
	if err != nil {
		return err
	}
	if eval.includeVertex != nil {
		err = eval.addEdges(*eval.includeVertex)
		if err != nil {
			return err
		}
	}
	err = eval.addAllEdges(eval.globals.vertices)
	if err != nil {
		return err
	}

	err = eval.globals.graph.Validate()
	if err != nil {
		return err
	}

	eval.globals.graph.TransitiveReduction()

	return nil
}

func (eval *configEvaluator) evaluateVariable(vertex variableVertex, diags hcl.Diagnostics, evaluateGlobals bool) bool {
	if vertex.Type == global && !evaluateGlobals {
		return false
	}

	if vertex.Evaluated {
		return true
	}

	valuesCty, err := eval.convertValuesToVariables()
	if err != nil {
		// TODO: diags.Extend(??)
		return false
	}

	ctx := hcl.EvalContext{
		Variables: valuesCty,
	}

	value, currentDiags := vertex.Expr.Value(&ctx)
	if currentDiags != nil && currentDiags.HasErrors() {
		_ = diags.Extend(currentDiags)
		return false
	}

	vertex.Evaluated = true

	switch vertex.Type {
	case global:
		eval.globals.values[vertex.Name] = value

	case local:
		eval.localValues[vertex.Name] = value

	default:
		// TODO: diags.Extend(??)
		return false
	}

	return true
}

func (eval *configEvaluator) getBlocks(file *hcl.File) (*hcl.Block, *hcl.Block, hcl.Diagnostics) {
	const locals = "locals"
	const globals = "globals"


	localsBlock, diags := getLocalsBlock(file)
	if diags != nil && diags.HasErrors() {
		return nil, nil, diags
	}

	globalsBlock, diags := getGlobalsBlock(file)
	if diags != nil && diags.HasErrors() {
		return nil, nil, diags
	}


	return localsBlock, globalsBlock, diags
}


func (eval *configEvaluator) addVertices(vertexType string, block hcl.Body, consumer func(vertex variableVertex) error) error {
	attrs, diags := block.JustAttributes()
	if diags != nil && diags.HasErrors() {
		return diags
	}

	for name, attr := range attrs {
		var vertex *variableVertex = nil

		if vertexType == global {
			globalVertex, exists := eval.globals.vertices[name]
			if exists && globalVertex.Expr == nil {
				// This was referenced by a child but not overridden there
				vertex = &globalVertex
				globalVertex.Evaluator = eval
				globalVertex.Expr = attr.Expr
			}
		}

		if vertex == nil {
			vertex = &variableVertex{
				Evaluator: eval,
				Type: vertexType,
				Name: name,
				Expr: attr.Expr,
				Evaluated: false,
			}
		}

		eval.globals.graph.Add(*vertex)
		err := consumer(*vertex)
		if err != nil {
			return err
		}
	}

	return nil
}

func (eval *configEvaluator) addAllEdges(vertices map[string]variableVertex) error {
	for _, vertex := range vertices {
		err := eval.addEdges(vertex)
		if err != nil {
			return err
		}
	}

	return nil
}

func (eval *configEvaluator) addEdges(target variableVertex) error {
	if target.Expr == nil {
		return nil
	}

	variables := target.Expr.Variables()

	if variables == nil || len(variables) <= 0 {
		eval.globals.graph.Connect(&basicEdge{
			S: eval.globals.root,
			T: target,
		})
		return nil
	}

	for _, variable := range variables {
		sourceType, sourceName, err := getVariableRootAndName(variable)
		if err != nil {
			return err
		}

		switch sourceType {
		case global:
			source, exists := eval.globals.vertices[sourceName]
			if !exists {
				// Could come from parent context, add empty node for now.
				source = variableVertex{
					Evaluator: nil,
					Type: global,
					Name: sourceName,
					Expr: nil,
					Evaluated: false,
				}
			}
			eval.globals.graph.Connect(&basicEdge{
				S: source,
				T: target,
			})
		case local:
			source, exists := eval.localVertices[sourceName]
			if !exists {
				// TODO: error
				return nil
			}

			eval.globals.graph.Connect(&basicEdge{
				S: source,
				T: target,
			})
		case include:
			// TODO validate options
			eval.globals.graph.Connect(&basicEdge{
				S: eval.includeVertex,
				T: target,
			})
		default:
			// TODO: error
			return nil
		}
	}

	return nil
}

func getVariableRootAndName(variable hcl.Traversal) (string, string, error) {
	// TODO: validation
	sourceType := variable.RootName()
	sourceName := variable[1].(hcl.TraverseAttr).Name
	return sourceType, sourceName, nil
}

func (eval *configEvaluator) convertValuesToVariables() (map[string]cty.Value, error) {
	values := map[string]map[string]cty.Value{
		local: eval.localValues,
		global: eval.globals.values,
	}

	variables := map[string]cty.Value{}
	for k, v := range values {
		variable, err := gocty.ToCtyValue(v, generateTypeFromMap(v))
		if err != nil {
			return nil, errors.WithStackTrace(err)
		}

		variables[k] = variable
	}

	return variables, nil
}

func (eval *configEvaluator) toResult() (*EvaluationResult, error) {
	variables, err := eval.convertValuesToVariables()
	if err != nil {
		return nil, err
	}

	return &EvaluationResult{
		ConfigFile: eval.configFile,
		Variables:  variables,
	}, nil
}

func (globals *evaluatorGlobals) evaluateVariables(evaluateGlobals bool) hcl.Diagnostics {
	diags := hcl.Diagnostics{}

	walkBreadthFirst(globals.graph, globals.root, func(v dag.Vertex) (shouldContinue bool) {
		if _, isRoot := v.(rootVertex); isRoot {
			return true
		}

		vertex, ok := v.(variableVertex)
		if !ok {
			// TODO: diags.Extend(??)
			return false
		}

		return vertex.Evaluator.evaluateVariable(vertex, diags, evaluateGlobals)
	})

	if diags.HasErrors() {
		return diags
	}

	return nil
}

func walkBreadthFirst(g dag.AcyclicGraph, root dag.Vertex, cb func(vertex dag.Vertex) (shouldContinue bool)) {
	visited := map[dag.Vertex]struct{}{}
	queue := []dag.Vertex{root}

	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:] // pop

		if _, contained := visited[v]; !contained {
			visited[v] = struct{}{}
			shouldContinue := cb(v)

			if shouldContinue {
				for _, child := range g.DownEdges(v).List() {
					queue = append(queue, child)
				}
			}
		}
	}
}

func generateTypeFromMap(value map[string]cty.Value) cty.Type {
	typeMap := map[string]cty.Type{}
	for k, v := range value {
		typeMap[k] = v.Type()
	}
	return cty.Object(typeMap)
}
