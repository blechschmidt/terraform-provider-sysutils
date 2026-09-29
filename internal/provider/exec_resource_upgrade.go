package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// execModelV0 is the state of sysutils_exec before schema version 1.
type execModelV0 struct {
	Command                  types.List   `tfsdk:"command"`
	Environment              types.Map    `tfsdk:"environment"`
	InheritParentEnvironment types.Bool   `tfsdk:"inherit_parent_environment"`
	WorkingDirectory         types.String `tfsdk:"working_directory"`
	Stdin                    types.String `tfsdk:"stdin"`
	Triggers                 types.Map    `tfsdk:"triggers"`
	FailOnNonzero            types.Bool   `tfsdk:"fail_on_nonzero"`
	ExitCode                 types.Int64  `tfsdk:"exit_code"`
	Stdout                   types.String `tfsdk:"stdout"`
	Stderr                   types.String `tfsdk:"stderr"`
	ID                       types.String `tfsdk:"id"`
}

func execSchemaV0() *schema.Schema {
	return &schema.Schema{
		Attributes: map[string]schema.Attribute{
			"command":                    schema.ListAttribute{Required: true, ElementType: types.StringType},
			"environment":                schema.MapAttribute{Optional: true, ElementType: types.StringType},
			"inherit_parent_environment": schema.BoolAttribute{Optional: true, Computed: true},
			"working_directory":          schema.StringAttribute{Optional: true},
			"stdin":                      schema.StringAttribute{Optional: true},
			"triggers":                   schema.MapAttribute{Optional: true, ElementType: types.StringType},
			"fail_on_nonzero":            schema.BoolAttribute{Optional: true, Computed: true},
			"exit_code":                  schema.Int64Attribute{Computed: true},
			"stdout":                     schema.StringAttribute{Computed: true},
			"stderr":                     schema.StringAttribute{Computed: true},
			"id":                         schema.StringAttribute{Computed: true},
		},
	}
}

// UpgradeState fills in the attributes added in version 1 with the values a
// new resource would get by default, so that upgrading the provider does not
// plan a replacement (and re-run) of every existing sysutils_exec.
func (r *execResource) UpgradeState(_ context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: execSchemaV0(),
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var old execModelV0
				resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
				if resp.Diagnostics.HasError() {
					return
				}
				// Version 0 stored the complete output, so hashing the stored
				// value yields the hash of the complete output.
				hashOf := func(s types.String) types.String {
					if s.IsNull() || s.IsUnknown() {
						return types.StringNull()
					}
					sum := sha256.Sum256([]byte(s.ValueString()))
					return types.StringValue(hex.EncodeToString(sum[:]))
				}
				upgraded := execModel{
					Command:                  old.Command,
					DestroyCommand:           types.ListNull(types.StringType),
					Environment:              old.Environment,
					InheritParentEnvironment: old.InheritParentEnvironment,
					WorkingDirectory:         old.WorkingDirectory,
					Stdin:                    old.Stdin,
					Triggers:                 old.Triggers,
					FailOnNonzero:            old.FailOnNonzero,
					Timeout:                  types.StringNull(),
					SensitiveOutput:          types.BoolValue(false),
					MaxOutputBytes:           types.Int64Value(defaultMaxOutputBytes),
					ExitCode:                 old.ExitCode,
					Stdout:                   old.Stdout,
					Stderr:                   old.Stderr,
					SensitiveStdout:          types.StringNull(),
					SensitiveStderr:          types.StringNull(),
					StdoutSHA256:             hashOf(old.Stdout),
					StderrSHA256:             hashOf(old.Stderr),
					Truncated:                types.BoolValue(false),
					ID:                       old.ID,
				}
				resp.Diagnostics.Append(resp.State.Set(ctx, &upgraded)...)
			},
		},
	}
}
