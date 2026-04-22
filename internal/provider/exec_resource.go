package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*execResource)(nil)

func NewExecResource() resource.Resource { return &execResource{} }

type execResource struct{}

type execModel struct {
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

func (r *execResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_exec"
}

func (r *execResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	requiresReplaceList := []planmodifier.List{listplanmodifier.RequiresReplace()}
	requiresReplaceMap := []planmodifier.Map{mapplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Executes a command. Re-runs when any input attribute (including `triggers`) changes.",
		Attributes: map[string]schema.Attribute{
			"command": schema.ListAttribute{
				Required:      true,
				ElementType:   types.StringType,
				Description:   "Command and arguments as a list (argv[0] is the executable).",
				PlanModifiers: requiresReplaceList,
			},
			"environment": schema.MapAttribute{
				Optional:      true,
				ElementType:   types.StringType,
				Description:   "Environment variables to set. If `inherit_parent_environment` is false, these are the only variables in the child environment.",
				PlanModifiers: requiresReplaceMap,
			},
			"inherit_parent_environment": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(true),
				Description:   "If true (default), the child starts from the provider process's environment and `environment` overrides specific keys. If false, only `environment` is used.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"working_directory": schema.StringAttribute{
				Optional:      true,
				Description:   "Working directory for the child process.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"stdin": schema.StringAttribute{
				Optional:      true,
				Description:   "Data to pipe into the command's standard input.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"triggers": schema.MapAttribute{
				Optional:      true,
				ElementType:   types.StringType,
				Description:   "Arbitrary map whose changes force re-execution.",
				PlanModifiers: requiresReplaceMap,
			},
			"fail_on_nonzero": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(true),
				Description:   "If true (default), a nonzero exit code causes the apply to fail. If false, the exit code is recorded and apply continues.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"exit_code": schema.Int64Attribute{
				Computed:    true,
				Description: "Exit code returned by the command.",
			},
			"stdout": schema.StringAttribute{
				Computed:    true,
				Description: "Captured standard output.",
			},
			"stderr": schema.StringAttribute{
				Computed:    true,
				Description: "Captured standard error.",
			},
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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

	var argv []string
	resp.Diagnostics.Append(plan.Command.ElementsAs(ctx, &argv, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(argv) == 0 {
		resp.Diagnostics.AddAttributeError(path.Root("command"), "Empty command", "command must contain at least one element")
		return
	}

	inherit := true
	if !plan.InheritParentEnvironment.IsNull() && !plan.InheritParentEnvironment.IsUnknown() {
		inherit = plan.InheritParentEnvironment.ValueBool()
	}

	extraEnv := map[string]string{}
	if !plan.Environment.IsNull() && !plan.Environment.IsUnknown() {
		resp.Diagnostics.Append(plan.Environment.ElementsAs(ctx, &extraEnv, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = buildEnv(inherit, extraEnv)
	if wd := plan.WorkingDirectory.ValueString(); wd != "" {
		cmd.Dir = wd
	}
	if !plan.Stdin.IsNull() && plan.Stdin.ValueString() != "" {
		cmd.Stdin = bytes.NewBufferString(plan.Stdin.ValueString())
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	switch e := runErr.(type) {
	case nil:
	case *exec.ExitError:
		exitCode = e.ExitCode()
	default:
		resp.Diagnostics.AddError("Command failed to start", fmt.Sprintf("%s: %s", argv[0], runErr.Error()))
		return
	}

	failOnNonzero := true
	if !plan.FailOnNonzero.IsNull() && !plan.FailOnNonzero.IsUnknown() {
		failOnNonzero = plan.FailOnNonzero.ValueBool()
	}
	if failOnNonzero && exitCode != 0 {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Command exited with non-zero status %d", exitCode),
			fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String()),
		)
		return
	}

	plan.ExitCode = types.Int64Value(int64(exitCode))
	plan.Stdout = types.StringValue(stdout.String())
	plan.Stderr = types.StringValue(stderr.String())
	plan.InheritParentEnvironment = types.BoolValue(inherit)
	plan.FailOnNonzero = types.BoolValue(failOnNonzero)
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

func (r *execResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All mutable inputs use RequiresReplace, so Update is a no-op passthrough.
	var plan execModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *execResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Nothing to clean up; the command already ran.
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
