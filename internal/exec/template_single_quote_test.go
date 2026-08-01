package exec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudposse/atmos/pkg/schema"
	u "github.com/cloudposse/atmos/pkg/utils"
	atmosYaml "github.com/cloudposse/atmos/pkg/yaml"
)

// Reproduction for https://github.com/cloudposse/atmos/issues/1250.
//
// A single quote (apostrophe) anywhere in Go template *output* breaks stack processing with
// "yaml: line N: did not find expected key".
//
// Mechanism (all three steps happen inside `processComponentSectionTemplates`):
//
//  1. The component section map is serialised back to YAML text before templates are rendered.
//     `yaml.v3` emits a value like `{{ toJson (datasource "x").y }}` as a *single-quoted* scalar,
//     because the value contains double quotes and starts with `{`:
//
//     agent: '{{ toJson (datasource "x").y }}'
//
//  2. `ProcessTmplWithDatasources` renders that YAML *as plain text*. The rendered JSON is
//     substituted verbatim, with no YAML re-quoting:
//
//     agent: '{"instruction":"the company's assistant"}'
//
//  3. The rendered text is parsed back as YAML (`template_utils.go` inside the evaluation loop,
//     then again in `processComponentSectionTemplates`). The apostrophe from step 2 terminates
//     the single-quoted scalar early, so the remainder is no longer valid YAML and parsing fails.
//
// The bug is in the render-into-YAML-text-then-reparse pipeline, so it is NOT specific to
// `!template`, to `toJson`, or to gomplate datasources — those are just what the reporter used.
// The sub-tests below pin each of those variants.
//
// These assertions document the CURRENT (broken) behaviour. When the escaping bug is fixed, they
// will fail loudly and must be flipped to assert the rendered values instead.
func TestProcessComponentSectionTemplates_SingleQuoteInTemplateOutput_Issue1250(t *testing.T) {
	// `printf "...%cs..." 39` emits an apostrophe from the *template*, never from the manifest
	// source. That keeps the input manifest itself valid YAML and isolates the rendered output as
	// the only thing under test.
	const apostropheTemplate = `{{ toJson (dict "instruction" (printf "the company%cs assistant" 39)) }}`

	// Same template, but the rendered text contains no apostrophe. Used as the control.
	const plainTemplate = `{{ toJson (dict "instruction" "the company assistant") }}`

	tests := []struct {
		name       string
		value      string
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:       "apostrophe in rendered template output fails",
			value:      apostropheTemplate,
			wantErr:    true,
			wantErrMsg: "did not find expected key",
		},
		{
			// Fidelity to the issue: the reporter wrapped the expression in `!template`. The
			// failure happens before `!template` is ever interpreted, so the tag changes nothing.
			name:       "apostrophe fails identically behind the !template tag",
			value:      u.AtmosYamlFuncTemplate + " " + apostropheTemplate,
			wantErr:    true,
			wantErrMsg: "did not find expected key",
		},
		{
			// Control: proves the apostrophe is the trigger, not `toJson`, not the tag, and not
			// the surrounding machinery.
			name:    "identical template without an apostrophe succeeds",
			value:   plainTemplate,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atmosConfig := &schema.AtmosConfiguration{
				Templates: schema.Templates{
					Settings: schema.TemplatesSettings{
						Enabled: true,
						Sprig:   schema.TemplatesSettingsSprig{Enabled: true},
					},
				},
			}
			info := &schema.ConfigAndStacksInfo{ComponentSection: map[string]any{}}

			componentSection := map[string]any{
				"vars": map[string]any{"bedrock_agent_configs": tt.value},
			}

			result, err := processComponentSectionTemplates(atmosConfig, info, componentSection, map[string]any{})

			if tt.wantErr {
				require.Error(t, err, "issue #1250 reproduction: expected the apostrophe to break YAML re-parsing")
				assert.ErrorContains(t, err, tt.wantErrMsg)
				return
			}

			require.NoError(t, err)
			vars, ok := result["vars"].(map[string]any)
			require.True(t, ok, "vars section must be a map, got %T", result["vars"])
			assert.Equal(t, `{"instruction":"the company assistant"}`, vars["bedrock_agent_configs"])
		})
	}
}

// TestProcessComponentSectionTemplates_SingleQuotedScalarIsTheCarrier_Issue1250 pins step 1 of the
// mechanism described above: that `yaml.v3` really does emit the template expression as a
// single-quoted scalar. If a future change switches this to double-quoted or literal style, the
// escaping analysis in issue #1250 no longer applies and this test should be revisited.
func TestProcessComponentSectionTemplates_SingleQuotedScalarIsTheCarrier_Issue1250(t *testing.T) {
	componentSection := map[string]any{
		"vars": map[string]any{
			"bedrock_agent_configs": `{{ toJson (datasource "vars_merged_final").bedrock_agent_configs }}`,
		},
	}

	// Default delimiters ({{ and }}) contain no single quote, so no delimiter-safety rewriting
	// kicks in and the standard yaml.v3 encoder decides the quoting style.
	yamlStr, err := atmosYaml.ConvertToYAMLPreservingDelimiters(componentSection, nil)
	require.NoError(t, err)

	assert.Contains(t, yamlStr,
		`bedrock_agent_configs: '{{ toJson (datasource "vars_merged_final").bedrock_agent_configs }}'`,
		"the template expression must be carried in a single-quoted YAML scalar for issue #1250 to occur")
}

// TestProcessTmplWithDatasources_SingleQuoteFromGomplateDatasource_Issue1250 reproduces the
// reporter's exact setup: a gomplate datasource whose JSON contains an apostrophe, pulled in with
// `toJson (datasource "...")`. It confirms the failure originates in the YAML re-parse inside
// `ProcessTmplWithDatasources`' evaluation loop, before `!template` decoding is ever reached.
//
// The reporter used `merge:` across two datasources; a single datasource is enough to trigger the
// bug, and keeps the reproduction minimal.
func TestProcessTmplWithDatasources_SingleQuoteFromGomplateDatasource_Issue1250(t *testing.T) {
	tests := []struct {
		name        string
		instruction string
		wantErr     bool
	}{
		{
			name:        "datasource value containing an apostrophe fails",
			instruction: "You are the company's assistant.",
			wantErr:     true,
		},
		{
			name:        "same datasource without an apostrophe succeeds",
			instruction: "You are the company assistant.",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Write the datasource JSON to a temp file and reference it by absolute file:// URL so
			// the test does not depend on the process working directory.
			dsPath := filepath.Join(t.TempDir(), "defaults.json")
			body, err := u.ConvertToJSON(map[string]any{
				"bedrock_agent_configs": map[string]any{
					"agent_one": map[string]any{"instruction": tt.instruction},
				},
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(dsPath, []byte(body), 0o600))

			atmosConfig := &schema.AtmosConfiguration{
				Templates: schema.Templates{
					Settings: schema.TemplatesSettings{
						Enabled:  true,
						Sprig:    schema.TemplatesSettingsSprig{Enabled: true},
						Gomplate: schema.TemplatesSettingsGomplate{Enabled: true},
					},
				},
			}
			templateSettings := schema.Settings{
				Templates: schema.Templates{
					Settings: schema.TemplatesSettings{
						Gomplate: schema.TemplatesSettingsGomplate{
							Enabled: true,
							Timeout: 5,
							Datasources: map[string]schema.TemplatesSettingsGomplateDatasource{
								"vars_defaults": {Url: "file://" + filepath.ToSlash(dsPath)},
							},
						},
					},
				},
			}

			// This is the YAML text the stack processor feeds to the template engine: a
			// single-quoted scalar carrying the template expression.
			tmplValue := "vars:\n" +
				`  bedrock_agent_configs: '{{ toJson (datasource "vars_defaults").bedrock_agent_configs }}'` + "\n"

			rendered, err := ProcessTmplWithDatasources(
				atmosConfig,
				&schema.ConfigAndStacksInfo{ComponentSection: map[string]any{}},
				templateSettings,
				"issue-1250",
				tmplValue,
				map[string]any{},
				true,
			)

			if tt.wantErr {
				require.Error(t, err, "issue #1250 reproduction: expected the apostrophe to break YAML re-parsing")
				assert.ErrorContains(t, err, "did not find expected key")
				return
			}

			require.NoError(t, err)
			assert.Contains(t, rendered, tt.instruction)
		})
	}
}
