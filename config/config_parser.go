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
}

// NewConfigParser creates a new parser, ready to parse configuration files.
func NewConfigParser() *ConfigParser {
	return &ConfigParser{
		Parser: hclparse.NewParser(),
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

func (cp *ConfigParser) ParseConfigFile(filename string) error {
	cp.Filename = filename
	err := cp.ReadFile(filename)
	if err != nil {
		return err
	}

	cp.File, err = parseHcl(cp.Parser, cp.FileContent, filename)
	if err != nil {
		return err
	}

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

	return nil
}

func (cp *ConfigParser) CreateParent(include *IncludeConfig) error {
	cp.Parent = NewConfigParser()
	cp.Parent.Options = cp.Options

	filename, err := cp.GetIncludeFilename(include)
	if err != nil {
		return err
	}

	err = cp.Parent.ParseConfigFile(filename)
	if err != nil {
		return err
	}

	err = cp.Parent.ProcessIncludes()
	if err != nil {
		return err
	}

	return nil
}

func (cp *ConfigParser) ProcessVariables() error {
	variables, _, err := ParseConfigVariables(cp)
	if err != nil {
		return err
	}
	localsAsCty := variables.Variables[local]
	globalsAsCty := variables.Variables[global]

	// Initialize evaluation context extensions from base blocks.
	cp.Context = EvalContextExtensions{
		Locals:  &localsAsCty,
		Globals: &globalsAsCty,
		Include: cp.IncludeConfig,
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

/*

// If this file includes another, parse and merge it.  Otherwise just return this config.
	/*if terragruntInclude.Include != nil {
		includedConfig, err := parseIncludedConfig(terragruntInclude.Include, terragruntOptions)
		if err != nil {
			return nil, err
		}
		return mergeConfigWithIncludedConfig(config, includedConfig, terragruntOptions)
	} else {*/
//return config, nil
//}
