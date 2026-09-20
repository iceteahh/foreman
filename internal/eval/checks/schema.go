package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaValidation validates result.structured_output against acceptance.json_schema
// independently of the CLI's own enforcement.
type SchemaValidation struct{}

func (SchemaValidation) Name() string { return "schema_validation" }

func (SchemaValidation) Run(_ context.Context, in *Input) Result {
	if !in.Task.Acceptance.HasSchema() {
		return na("task has no json_schema")
	}
	if in.Result == nil || in.Result.Result == nil {
		return na("no result event")
	}
	out := in.Result.Result.StructuredOutput
	if len(out) == 0 || strings.TrimSpace(string(out)) == "null" {
		return fail("result carries no structured_output although the task sets json_schema")
	}
	if err := ValidateJSON(in.Task.Acceptance.JSONSchema, out); err != nil {
		return fail("structured_output does not match json_schema:\n" + err.Error())
	}
	return pass(fmt.Sprintf("structured_output valid (%d bytes)", len(out)))
}

// ValidateJSON checks instance against schema, both raw JSON.
func ValidateJSON(schema, instance []byte) error {
	schemaDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	if err != nil {
		return fmt.Errorf("schema is not JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("acceptance.json", schemaDoc); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	s, err := c.Compile("acceptance.json")
	if err != nil {
		return fmt.Errorf("schema does not compile: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(instance)))
	if err != nil {
		return fmt.Errorf("structured_output is not JSON: %w", err)
	}
	return s.Validate(inst)
}
