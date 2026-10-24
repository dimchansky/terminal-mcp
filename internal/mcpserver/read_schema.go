package mcpserver

import (
	"github.com/google/jsonschema-go/jsonschema"

	"terminalmcp/internal/filesystem"
)

// fileReadSchema keeps client-side validation aligned with file_read limits.
// The SDK uses jsonschema tags as descriptions; numeric bounds must be set
// explicitly. A zero max_bytes value requests the default read budget.
func fileReadSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[filesystem.ReadArgs](nil)
	if err != nil {
		panic(err)
	}
	files := schema.Properties["files"]
	files.MinItems = jsonschema.Ptr(1)
	files.MaxItems = jsonschema.Ptr(8)

	size := schema.Properties["max_bytes"]
	size.Minimum = jsonschema.Ptr(float64(0))
	size.Maximum = jsonschema.Ptr(float64(filesystem.MaxReadBytes))

	if files.Items != nil {
		for _, name := range []string{"start_line", "end_line"} {
			if field := files.Items.Properties[name]; field != nil {
				field.Minimum = jsonschema.Ptr(float64(0))
			}
		}
	}
	return schema
}
