package config

import (
	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"path/filepath"
)

type ConfigParser struct {
	Parser        *hclparse.Parser
	FileContent   string
	Filename      string
	File          *hcl.File
	Options       *options.TerragruntOptions
	Parent        *ConfigParser
	Include       *terragruntInclude
	IncludeConfig *IncludeConfig
	Context       EvalContextExtensions
	Config        *TerragruntConfig
	Eval          *configEvaluator
}

// NewConfigParser creates a new parser, ready to parse configuration files.
func NewConfigParser() *ConfigParser {
	return &ConfigParser{
		Parser:  hclparse.NewParser(),
		Context: EvalContextExtensions{},
	}
}

// Read the Terragrunt config file at the given path.
func (cp *ConfigParser) ReadFile(filename string) error {
	configString, err := util.ReadFileAsString(filename)
	if err != nil {
		return err
	}
	cp.FileContent = configString
	return nil
}

func (cp *ConfigParser) GetIncludeFilename(includedConfig *IncludeConfig) (string, error) {
	if includedConfig.Path == "" {
		return "", errors.WithStackTrace(IncludedConfigMissingPath(cp.Options.TerragruntConfigPath))
	}

	includePath := includedConfig.Path

	if !filepath.IsAbs(includePath) {
		includePath = util.JoinPath(filepath.Dir(cp.Options.TerragruntConfigPath), includePath)
	}

	return includePath, nil;
}

func (cp *ConfigParser) ParseConfigFile() error {
	err := cp.ReadFile(cp.Filename)
	if err != nil {
		return err
	}
	return cp.ParseConfigString()
}

func (cp *ConfigParser) ParseConfigString() error {
	file, err := parseHcl(cp.Parser, cp.FileContent, cp.Filename)
	if err != nil {
		return err
	}
	cp.File = file
	return nil
}

func (cp *ConfigParser) ProcessIncludes() error {

	// Decode just the `include` block, and verify that it's allowed here
	terragruntInclude, err := decodeAsTerragruntInclude(
		cp.File,
		cp.Filename,
		cp.Options,
		EvalContextExtensions{},
	)
	if err != nil {
		return err
	}

	includeForDecode, err := getIncludedConfigForDecode(terragruntInclude, cp.Options, cp.IncludeConfig)
	if err != nil {
		return err
	}

	cp.Include = terragruntInclude
	cp.IncludeConfig = includeForDecode

	if cp.Include.Include != nil {
		err = cp.CreateParent(cp.Include.Include)
		if err != nil {
			return err
		}
	}
	cp.Context.Include = cp.IncludeConfig
	return nil
}

func (cp *ConfigParser) CreateParent(include *IncludeConfig) error {
	cp.Parent = NewConfigParser()
	cp.Parent.Options = cp.Options

	filename, err := cp.GetIncludeFilename(include)
	if err != nil {
		return err
	}

	cp.Parent.Filename = filename
	err = cp.Parent.ParseConfigFile()
	if err != nil {
		return err
	}

	err = cp.Parent.ProcessIncludes()
	if err != nil {
		return err
	}

	return nil
}

func (cp *ConfigParser) PreprocessVariables(globals *evaluatorGlobals) error {
	cp.Eval = newConfigEvaluator(cp)
	cp.Eval.setGlobals(globals)

	err := cp.Eval.decodeVariables()
	if err != nil {
		return err
	}

	err = cp.Eval.evaluateLocals()
	if err != nil {
		return err
	}

	if cp.Parent != nil {
		err := cp.Parent.PreprocessVariables(cp.Eval.globals)
		if err != nil {
			return err
		}

	}

	// Only validate the graph for the child config file
	err = cp.Eval.processEdges(globals == nil)
	if err != nil {
		return err
	}

	return nil
}

func (cp *ConfigParser) EvaluateVariables(localsOnly bool) error {
	if cp.Parent != nil {
		err := cp.Parent.EvaluateVariables(true)
		if err != nil {
			return err
		}
	}

	if localsOnly {
		err := cp.Eval.evaluateLocals()
		if err != nil {
			return err
		}
	} else {
		err := cp.Eval.evaluateAllVariables()
		if err != nil {
			return err
		}
	}

	return nil
}

func (cp *ConfigParser) ProcessVariables(globals *evaluatorGlobals) error {

	err := cp.PreprocessVariables(nil)
	if err != nil {
		return err
	}

	err = cp.EvaluateVariables(false)
	if err != nil {
		return err
	}
	err = cp.Eval.evaluateAllVariables()
	if err != nil {
		return err
	}

	err = cp.SetVariables()
	if err != nil {
		return err
	}

	return nil
}

func (cp *ConfigParser) SetVariables() error {
	variables, err := cp.Eval.toResult()
	if err != nil {
		return err
	}
	localsAsCty := variables.Variables[local]
	globalsAsCty := variables.Variables[global]
	cp.Context.Locals = &localsAsCty
	cp.Context.Globals = &globalsAsCty

	if cp.Parent != nil {
		return cp.Parent.SetVariables()
	}

	return nil
}


func (cp *ConfigParser) ProcessDependencies() error {
	// Decode the `dependency` blocks, retrieving the outputs from the target terragrunt config in the
	// process.
	retrievedOutputs, err := decodeAndRetrieveOutputs(cp.File, cp.Filename, cp.Options, cp.Context)
	if err != nil {
		return err
	}

	cp.Context.DecodedDependencies = retrievedOutputs
	return nil
}

func (cp *ConfigParser) ProcessRemainder() error {
	// Decode the rest of the config, passing in this config's `include` block or the child's `include` block, whichever
	// is appropriate
	configFile, err := decodeAsTerragruntConfigFile(cp.File, cp.Filename, cp.Options, cp.Context)
	if err != nil {
		return err
	}
	if configFile == nil {
		return errors.WithStackTrace(CouldNotResolveTerragruntConfigInFile(cp.Filename))
	}

	cp.Config, err = convertToTerragruntConfig(configFile, cp.Filename, cp.Options, cp.Context)
	if err != nil {
		return err
	}

	return nil
}

func (cp *ConfigParser) MergeWithParent() (*TerragruntConfig, error) {
	parentConfig, err := cp.Parent.Finalize()
	if err != nil {
		return nil, err
	}

    return mergeConfigWithIncludedConfig(cp.Config, parentConfig, cp.Options)
}

func (cp *ConfigParser) Finalize() (*TerragruntConfig, error) {

	err := cp.ProcessDependencies()
	if err != nil {
		return nil, err
	}

	err = cp.ProcessRemainder()
	if err != nil {
		return nil, err
	}

	if cp.Parent != nil {
		return cp.MergeWithParent()
	}

	return cp.Config, nil
}