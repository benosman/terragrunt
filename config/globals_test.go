package config

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty/gocty"

	"github.com/gruntwork-io/terragrunt/errors"
)

func TestEvaluateGlobalsBlock(t *testing.T) {
	t.Parallel()

	terragruntOptions := mockOptionsForTest(t)
	mockFilename := "terragrunt.hcl"

	parser := hclparse.NewParser()
	file, err := parseHcl(parser, GlobalsTestConfig, mockFilename)
	require.NoError(t, err)

	evaluatedGlobals, err := evaluateGlobalsBlock(terragruntOptions, parser, file, mockFilename, nil)
	require.NoError(t, err)

	var actualRegion string
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["region"], &actualRegion))
	assert.Equal(t, actualRegion, "us-east-1")

	var actualS3Url string
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["s3_url"], &actualS3Url))
	assert.Equal(t, actualS3Url, "com.amazonaws.us-east-1.s3")

	var actualX float64
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["x"], &actualX))
	assert.Equal(t, actualX, float64(1))

	var actualY float64
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["y"], &actualY))
	assert.Equal(t, actualY, float64(2))

	var actualZ float64
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["z"], &actualZ))
	assert.Equal(t, actualZ, float64(3))

	var actualFoo struct{ First Foo }
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["foo"], &actualFoo))
	assert.Equal(t, actualFoo.First, Foo{
		Region: "us-east-1",
		Foo:    "bar",
	})

	var actualBar string
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["bar"], &actualBar))
	assert.Equal(t, actualBar, "us-east-1")
}

func TestEvaluateGlobalsBlockMultiDeepReference(t *testing.T) {
	t.Parallel()

	terragruntOptions := mockOptionsForTest(t)
	mockFilename := "terragrunt.hcl"

	parser := hclparse.NewParser()
	file, err := parseHcl(parser, GlobalsTestMultiDeepReferenceConfig, mockFilename)
	require.NoError(t, err)

	evaluatedGlobals, err := evaluateGlobalsBlock(terragruntOptions, parser, file, mockFilename, nil)
	require.NoError(t, err)

	expected := "a"

	var actualA string
	require.NoError(t, gocty.FromCtyValue(evaluatedGlobals["a"], &actualA))
	assert.Equal(t, actualA, expected)

	testCases := []string{
		"b",
		"c",
		"d",
		"e",
		"f",
		"g",
		"h",
		"i",
		"j",
	}
	for _, testCase := range testCases {
		expected = fmt.Sprintf("%s/%s", expected, testCase)

		var actual string
		require.NoError(t, gocty.FromCtyValue(evaluatedGlobals[testCase], &actual))
		assert.Equal(t, actual, expected)
	}
}

func TestEvaluateGlobalsBlockImpossibleWillFail(t *testing.T) {
	t.Parallel()

	terragruntOptions := mockOptionsForTest(t)
	mockFilename := "terragrunt.hcl"

	parser := hclparse.NewParser()
	file, err := parseHcl(parser, GlobalsTestImpossibleConfig, mockFilename)
	require.NoError(t, err)

	_, err = evaluateGlobalsBlock(terragruntOptions, parser, file, mockFilename, nil)
	require.Error(t, err)

	switch errors.Unwrap(err).(type) {
	case CouldNotEvaluateAllGlobalsError:
	default:
		t.Fatalf("Did not get expected error: %s", err)
	}
}

func TestEvaluateGlobalsBlockMultipleGlobalsBlocksWillFail(t *testing.T) {
	t.Parallel()

	terragruntOptions := mockOptionsForTest(t)
	mockFilename := "terragrunt.hcl"

	parser := hclparse.NewParser()
	file, err := parseHcl(parser, MultipleGlobalsBlockConfig, mockFilename)
	require.NoError(t, err)

	_, err = evaluateGlobalsBlock(terragruntOptions, parser, file, mockFilename, nil)
	require.Error(t, err)
}

const GlobalsTestConfig = `
globals {
  region = "us-east-1"

  // Simple reference
  s3_url = "com.amazonaws.${global.region}.s3"

  // Nested reference
  foo = [
    merge(
      {region = global.region},
	  {foo = "bar"},
	)
  ]
  bar = global.foo[0]["region"]

  // Multiple references
  x = 1
  y = 2
  z = global.x + global.y
}
`

const GlobalsTestMultiDeepReferenceConfig = `
# 10 chains deep
globals {
  a = "a"
  b = "${global.a}/b"
  c = "${global.b}/c"
  d = "${global.c}/d"
  e = "${global.d}/e"
  f = "${global.e}/f"
  g = "${global.f}/g"
  h = "${global.g}/h"
  i = "${global.h}/i"
  j = "${global.i}/j"
}
`

const GlobalsTestImpossibleConfig = `
globals {
  a = global.b
  b = global.a
}
`

const MultipleGlobalsBlockConfig = `
globals {
  a = "a"
}

globals {
  b = "b"
}
`
