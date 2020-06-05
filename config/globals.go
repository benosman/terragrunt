package config

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/gruntwork-io/terragrunt/options"
)

func decodeAsGlobals(
	file *hcl.File,
	filename string,
	terragruntOptions *options.TerragruntOptions,
	extensions EvalContextExtensions,
) (*cty.Value, error) {
	decodedGlobals := terragruntGlobals{}
	err := decodeHcl(file, filename, &decodedGlobals, terragruntOptions, extensions)
	if err != nil {
		return nil, err
	}
	return decodedGlobals.Globals, nil
}