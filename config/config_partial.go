package config

import (
	"fmt"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"

	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
)

// PartialDecodeSectionType is an enum that is used to list out which blocks/sections of the terragrunt config should be
// decoded in a partial decode.
type PartialDecodeSectionType int

const (
	DependenciesBlock PartialDecodeSectionType = iota
	DependencyBlock
	TerraformBlock
	TerragruntFlags
	TerragruntVersionConstraints
)

// terragruntInclude is a struct that can be used to only decode the include block.
type terragruntInclude struct {
	Include *IncludeConfig `hcl:"include,block"`
	Remain  hcl.Body       `hcl:",remain"`
}

// terragruntDependencies is a struct that can be used to only decode the dependencies block.
type terragruntDependencies struct {
	Dependencies *ModuleDependencies `hcl:"dependencies,block"`
	Remain       hcl.Body            `hcl:",remain"`
}

// terragruntTerraform is a struct that can be used to only decode the terraform block
type terragruntTerraform struct {
	Terraform *TerraformConfig `hcl:"terraform,block"`
	Remain    hcl.Body         `hcl:",remain"`
}

// terragruntFlags is a struct that can be used to only decode the flag attributes (skip and prevent_destroy)
type terragruntFlags struct {
	PreventDestroy *bool    `hcl:"prevent_destroy,attr"`
	Skip           *bool    `hcl:"skip,attr"`
	Remain         hcl.Body `hcl:",remain"`
}

// terragruntVersionConstraints is a struct that can be used to only decode the attributes related to constraining the
// versions of terragrunt and terraform.
type terragruntVersionConstraints struct {
	TerragruntVersionConstraint *string  `hcl:"terragrunt_version_constraint,attr"`
	TerraformVersionConstraint  *string  `hcl:"terraform_version_constraint,attr"`
	TerraformBinary             *string  `hcl:"terraform_binary,attr"`
	Remain                      hcl.Body `hcl:",remain"`
}

// terragruntDependency is a struct that can be used to only decode the dependency blocks in the terragrunt config
type terragruntDependency struct {
	Dependencies []Dependency `hcl:"dependency,block"`
	Remain       hcl.Body     `hcl:",remain"`
}

// DecodeBaseBlocks takes in a parsed HCL2 file and decodes the base blocks. Base blocks are blocks that should always
// be decoded even in partial decoding, because they provide bindings that are necessary for parsing any block in the
// file. Currently base blocks are:
// - locals
// - include
func DecodeBaseBlocks(
	terragruntOptions *options.TerragruntOptions,
	parser *hclparse.Parser,
	hclFile *hcl.File,
	filename string,
	includeFromChild *IncludeConfig,
) (*cty.Value, *terragruntInclude, *IncludeConfig, error) {
	// Decode just the `include` block, and verify that it's allowed here
	terragruntInclude, err := decodeAsTerragruntInclude(
		hclFile,
		filename,
		terragruntOptions,
		EvalContextExtensions{},
	)
	if err != nil {
		return nil, nil, nil, err
	}
	includeForDecode, err := getIncludedConfigForDecode(terragruntInclude, terragruntOptions, includeFromChild)
	if err != nil {
		return nil, nil, nil, err
	}

	// Evaluate all the expressions in the locals block separately and generate the variables list to use in the
	// evaluation context.
	locals, err := evaluateLocalsBlock(terragruntOptions, parser, hclFile, filename, includeForDecode)
	if err != nil {
		return nil, nil, nil, err
	}
	localsAsCty, err := convertValuesMapToCtyVal(locals)
	if err != nil {
		return nil, nil, nil, err
	}

	return &localsAsCty, terragruntInclude, includeForDecode, nil
}

func PartialParseConfigFile(
	filename string,
	terragruntOptions *options.TerragruntOptions,
	include *IncludeConfig,
	decodeList []PartialDecodeSectionType,
) (*TerragruntConfig, error) {
	configString, err := util.ReadFileAsString(filename)
	if err != nil {
		return nil, err
	}

	config, err := PartialParseConfigString(configString, terragruntOptions, include, filename, decodeList)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// ParitalParseConfigString partially parses and decodes the provided string. Which blocks/attributes to decode is
// controlled by the function parameter decodeList. These blocks/attributes are parsed and set on the output
// TerragruntConfig. Valid values are:
// - DependenciesBlock: Parses the `dependencies` block in the config
// - DependencyBlock: Parses the `dependency` block in the config
// - TerraformBlock: Parses the `terraform` block in the config
// - TerragruntFlags: Parses the boolean flags `prevent_destroy` and `skip` in the config
// - TerragruntVersionConstraints: Parses the attributes related to constraining terragrunt and terraform versions in
//                                 the config.
// Note that the following blocks are always decoded:
// - locals
// - include
// Note also that the following blocks are never decoded in a partial parse:
// - inputs
func PartialParseConfigString(
	configString string,
	terragruntOptions *options.TerragruntOptions,
	includeFromChild *IncludeConfig,
	filename string,
	decodeList []PartialDecodeSectionType,
) (*TerragruntConfig, error) {

	configParser := NewConfigParser()
	configParser.Options = terragruntOptions
	configParser.Filename = filename
	configParser.FileContent = configString

	return configParser.ProcessPartial(decodeList)
}

func partialParseIncludedConfig(includedConfig *IncludeConfig, terragruntOptions *options.TerragruntOptions, decodeList []PartialDecodeSectionType) (*TerragruntConfig, error) {
	if includedConfig.Path == "" {
		return nil, errors.WithStackTrace(IncludedConfigMissingPath(terragruntOptions.TerragruntConfigPath))
	}

	includePath := includedConfig.Path

	if !filepath.IsAbs(includePath) {
		includePath = util.JoinPath(filepath.Dir(terragruntOptions.TerragruntConfigPath), includePath)
	}

	return PartialParseConfigFile(
		includePath,
		terragruntOptions,
		includedConfig,
		decodeList,
	)
}

// This decodes only the `include` block of a terragrunt config, so its value can be used while decoding the rest of the
// config.
// For consistency, `include` in the call to `decodeHcl` is always assumed to be nil.
// Either it really is nil (parsing the child config), or it shouldn't be used anyway (the parent config shouldn't have
// an include block)
func decodeAsTerragruntInclude(
	file *hcl.File,
	filename string,
	terragruntOptions *options.TerragruntOptions,
	extensions EvalContextExtensions,
) (*terragruntInclude, error) {
	terragruntInclude := terragruntInclude{}
	err := decodeHcl(file, filename, &terragruntInclude, terragruntOptions, extensions)
	if err != nil {
		return nil, err
	}
	return &terragruntInclude, nil
}

// Custom error types

type InvalidPartialBlockName struct {
	sectionCode PartialDecodeSectionType
}

func (err InvalidPartialBlockName) Error() string {
	return fmt.Sprintf("Unrecognized partial block code %d. This is most likely an error in terragrunt. Please file a bug report on the project repository.", err.sectionCode)
}
