package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                 = (*execResource)(nil)
	_ resource.ResourceWithUpgradeState = (*execResource)(nil)
)

func NewExecResource() resource.Resource { return &execResource{} }

type execResource struct{}

type execModel struct {
	Command                  types.List   `tfsdk:"command"`
	DestroyCommand           types.List   `tfsdk:"destroy_command"`
	Environment              types.Map    `tfsdk:"environment"`
	InheritParentEnvironment types.Bool   `tfsdk:"inherit_parent_environment"`
	WorkingDirectory         types.String `tfsdk:"working_directory"`
	Stdin                    types.String `tfsdk:"stdin"`
	Triggers                 types.Map    `tfsdk:"triggers"`
	FailOnNonzero            types.Bool   `tfsdk:"fail_on_nonzero"`
	Timeout                  types.String `tfsdk:"timeout"`
	SensitiveOutput          types.Bool   `tfsdk:"sensitive_output"`
	MaxOutputBytes           types.Int64  `tfsdk:"max_output_bytes"`
	ExitCode                 types.Int64  `tfsdk:"exit_code"`
	Stdout                   types.String `tfsdk:"stdout"`
	Stderr                   types.String `tfsdk:"stderr"`
	SensitiveStdout          types.String `tfsdk:"sensitive_stdout"`
	SensitiveStderr          types.String `tfsdk:"sensitive_stderr"`
	StdoutSHA256             types.String `tfsdk:"stdout_sha256"`
	StderrSHA256             types.String `tfsdk:"stderr_sha256"`
	Truncated                types.Bool   `tfsdk:"truncated"`
	ID                       types.String `tfsdk:"id"`
}

func (r *execResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_exec"
}

func (r *execResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	requiresReplaceList := []planmodifier.List{listplanmodifier.RequiresReplace()}
	requiresReplaceMap := []planmodifier.Map{mapplanmodifier.RequiresReplace()}
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		// Version 1 added timeout, sensitive_output, destroy_command,
		// max_output_bytes and the output hash/truncation attributes.
		Version: 1,
		MarkdownDescription: "Runs a command when the resource is created and again whenever any input attribute changes. " +
			"The exit code, standard output, and standard error are recorded as computed attributes you can reference from other resources or outputs. " +
			"An optional `destroy_command` runs when the resource is destroyed.",
		Attributes: map[string]schema.Attribute{
			"command": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Command and arguments as a list. Element `0` is the executable. Example: `[\"/bin/sh\", \"-c\", \"echo hi\"]`.",
				Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
				PlanModifiers:       requiresReplaceList,
			},
			"destroy_command": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Command and arguments to run when the resource is destroyed, including when it is replaced. " +
					"It uses the same `environment`, `inherit_parent_environment`, `working_directory` and `timeout` as `command`, but no `stdin`. " +
					"A non-zero exit fails the destroy (and keeps the resource in state) unless `fail_on_nonzero` is `false`. " +
					"Changing it updates the resource in place without re-running `command`, so a broken destroy command can be fixed before destroying.",
				Validators: []validator.List{listvalidator.SizeAtLeast(1)},
			},
			"environment": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Environment variables to pass to the child. " +
					"If `inherit_parent_environment` is `false`, these are the only variables in the child's environment.",
				PlanModifiers: requiresReplaceMap,
			},
			"inherit_parent_environment": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "If `true` (default), the child process starts from the Terraform provider's environment and the keys in `environment` override specific values. " +
					"If `false`, only the `environment` map is used.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"working_directory": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Working directory for the child process.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"stdin": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Data to pipe into the command's standard input.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"triggers": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Arbitrary map whose changes force re-execution. " +
					"The values are not passed to the command; they exist solely to invalidate the resource.",
				PlanModifiers: requiresReplaceMap,
			},
			"fail_on_nonzero": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "If `true` (default), a non-zero exit code of `command` or `destroy_command` causes the apply or destroy to fail with the captured output in the diagnostic. " +
					"If `false`, the exit code is recorded and the apply continues; a failing `destroy_command` is ignored.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Maximum run time of `command` and `destroy_command`, as a Go duration string such as `\"30s\"`, `\"5m\"` or `\"1h30m\"`. " +
					"When it expires, the command's whole process group is killed with `SIGKILL` and the apply or destroy fails, regardless of `fail_on_nonzero`. " +
					"Unset means no timeout. Changing it updates the resource in place without re-running `command`.",
				Validators: []validator.String{positiveDuration()},
			},
			"sensitive_output": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "If `true`, the captured output is stored in `sensitive_stdout` and `sensitive_stderr`, which Terraform redacts in plans and CLI output, and `stdout` and `stderr` are left null. " +
					"Error diagnostics then omit the output as well. Defaults to `false`.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"max_output_bytes": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultMaxOutputBytes),
				MarkdownDescription: fmt.Sprintf("Maximum number of bytes of standard output, and separately of standard error, to store in state. Defaults to `%d` (1 MiB). ", defaultMaxOutputBytes) +
					"Output beyond the limit is discarded, a line starting with `[sysutils_exec: output truncated` is appended, and `truncated` is set. " +
					"The command itself is not affected, and the hashes always cover the complete output. `0` stores no output at all.",
				Validators:    []validator.Int64{int64validator.AtLeast(0)},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"exit_code": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Exit code returned by the command.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"stdout": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Captured standard output, capped at `max_output_bytes`. Null if `sensitive_output` is `true`.",
				PlanModifiers:       keepString,
			},
			"stderr": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Captured standard error, capped at `max_output_bytes`. Null if `sensitive_output` is `true`.",
				PlanModifiers:       keepString,
			},
			"sensitive_stdout": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "Captured standard output, capped at `max_output_bytes`, if `sensitive_output` is `true`; null otherwise.",
				PlanModifiers:       keepString,
			},
			"sensitive_stderr": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "Captured standard error, capped at `max_output_bytes`, if `sensitive_output` is `true`; null otherwise.",
				PlanModifiers:       keepString,
			},
			"stdout_sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded SHA-256 of the complete standard output, including any part beyond `max_output_bytes`.",
				PlanModifiers:       keepString,
			},
			"stderr_sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded SHA-256 of the complete standard error, including any part beyond `max_output_bytes`.",
				PlanModifiers:       keepString,
			},
			"truncated": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether standard output or standard error exceeded `max_output_bytes` and was truncated in state.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"id": schema.StringAttribute{
				MarkdownDescription: "Opaque resource identifier.",
				Computed:            true,
				PlanModifiers:       keepString,
			},
		},
	}
}

func (r *execResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan execModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec, diags := execSpecFor(ctx, &plan, plan.Command)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec.Stdin = plan.Stdin.ValueString()

	res, err := runCommand(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddError("Command failed to run", err.Error())
		return
	}

	sensitive := plan.SensitiveOutput.ValueBool()
	if res.TimedOut {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Command timed out after %s", spec.Timeout),
			fmt.Sprintf("%s did not finish within the timeout; its process group was killed.\n\n%s", spec.Argv[0], describeOutput(res, sensitive)),
		)
		return
	}
	if plan.FailOnNonzero.ValueBool() && res.ExitCode != 0 {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Command exited with non-zero status %d", res.ExitCode),
			describeOutput(res, sensitive),
		)
		return
	}

	plan.ExitCode = types.Int64Value(int64(res.ExitCode))
	plan.Stdout, plan.Stderr = types.StringNull(), types.StringNull()
	plan.SensitiveStdout, plan.SensitiveStderr = types.StringNull(), types.StringNull()
	if sensitive {
		plan.SensitiveStdout = types.StringValue(res.Stdout.String())
		plan.SensitiveStderr = types.StringValue(res.Stderr.String())
	} else {
		plan.Stdout = types.StringValue(res.Stdout.String())
		plan.Stderr = types.StringValue(res.Stderr.String())
	}
	plan.StdoutSHA256 = types.StringValue(res.Stdout.SHA256())
	plan.StderrSHA256 = types.StringValue(res.Stderr.SHA256())
	plan.Truncated = types.BoolValue(res.Truncated())
	plan.ID = types.StringValue(fmt.Sprintf("%d", os.Getpid()) + "-" + randomID())

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *execResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Exec results are immutable after creation; nothing to refresh.
	var state execModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update handles the attributes that do not force replacement (timeout and
// destroy_command). The command is not re-run, so the results of the original
// run are carried over from state.
func (r *execResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state execModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ExitCode = state.ExitCode
	plan.Stdout = state.Stdout
	plan.Stderr = state.Stderr
	plan.SensitiveStdout = state.SensitiveStdout
	plan.SensitiveStderr = state.SensitiveStderr
	plan.StdoutSHA256 = state.StdoutSHA256
	plan.StderrSHA256 = state.StderrSHA256
	plan.Truncated = state.Truncated
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *execResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state execModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.DestroyCommand.IsNull() {
		return
	}

	spec, diags := execSpecFor(ctx, &state, state.DestroyCommand)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	res, err := runCommand(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("destroy_command"), "Destroy command failed to run", err.Error())
		return
	}

	// The output of the destroy command is not stored anywhere, so it is only
	// ever surfaced in diagnostics.
	sensitive := state.SensitiveOutput.ValueBool()
	if res.TimedOut {
		resp.Diagnostics.AddAttributeError(
			path.Root("destroy_command"),
			fmt.Sprintf("Destroy command timed out after %s", spec.Timeout),
			fmt.Sprintf("%s did not finish within the timeout; its process group was killed. The resource was kept in state.\n\n%s", spec.Argv[0], describeOutput(res, sensitive)),
		)
		return
	}
	if res.ExitCode != 0 {
		// A null fail_on_nonzero can only come from a state that predates the
		// attribute's default, so treat it as the default.
		if state.FailOnNonzero.IsNull() || state.FailOnNonzero.ValueBool() {
			resp.Diagnostics.AddAttributeError(
				path.Root("destroy_command"),
				fmt.Sprintf("Destroy command exited with non-zero status %d", res.ExitCode),
				"The resource was kept in state. Fix destroy_command (changing it does not re-run command) or set fail_on_nonzero = false to ignore failures.\n\n"+
					describeOutput(res, sensitive),
			)
			return
		}
		resp.Diagnostics.AddAttributeWarning(
			path.Root("destroy_command"),
			fmt.Sprintf("Destroy command exited with non-zero status %d", res.ExitCode),
			"The failure was ignored because fail_on_nonzero is false.\n\n"+describeOutput(res, sensitive),
		)
	}
}

// execSpecFor builds the invocation of argvList using the environment,
// working directory, timeout and output cap configured in m.
func execSpecFor(ctx context.Context, m *execModel, argvList types.List) (execSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	var spec execSpec

	diags.Append(argvList.ElementsAs(ctx, &spec.Argv, false)...)
	if diags.HasError() {
		return spec, diags
	}
	if len(spec.Argv) == 0 {
		diags.AddError("Empty command", "The command list must contain at least one element.")
		return spec, diags
	}

	extraEnv := map[string]string{}
	if !m.Environment.IsNull() && !m.Environment.IsUnknown() {
		diags.Append(m.Environment.ElementsAs(ctx, &extraEnv, false)...)
		if diags.HasError() {
			return spec, diags
		}
	}
	inherit := m.InheritParentEnvironment.IsNull() || m.InheritParentEnvironment.ValueBool()
	spec.Env = buildEnv(inherit, extraEnv)
	spec.Dir = m.WorkingDirectory.ValueString()

	if t := m.Timeout.ValueString(); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d <= 0 {
			diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", fmt.Sprintf("Timeout %q is not a positive duration.", t))
			return spec, diags
		}
		spec.Timeout = d
	}

	spec.MaxOutputBytes = defaultMaxOutputBytes
	if !m.MaxOutputBytes.IsNull() && !m.MaxOutputBytes.IsUnknown() {
		spec.MaxOutputBytes = m.MaxOutputBytes.ValueInt64()
	}
	return spec, diags
}

// describeOutput renders the captured output for an error diagnostic. With
// sensitive output only the hashes are shown, since diagnostics are printed
// in CI logs.
func describeOutput(res *execResult, sensitive bool) string {
	if sensitive {
		return fmt.Sprintf("Output withheld because sensitive_output is true.\nstdout_sha256: %s\nstderr_sha256: %s",
			res.Stdout.SHA256(), res.Stderr.SHA256())
	}
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", res.Stdout.String(), res.Stderr.String())
}

func buildEnv(inherit bool, extra map[string]string) []string {
	merged := map[string]string{}
	if inherit {
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				merged[kv[:i]] = kv[i+1:]
			}
		}
	}
	for k, v := range extra {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}
