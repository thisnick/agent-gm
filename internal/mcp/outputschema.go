package mcp

import (
	"github.com/thisnick/agent-gm/internal/api"
)

// The `outputSchema` every tool declares (spec section 8.2).
//
// It describes the envelope the tool really returns -- `{data, next_cursor,
// warnings}`, which is the REST envelope minus the request ID -- with `data`
// typed by that tool's own DTO. The DTO half is reflected off the very struct
// the REST handler serialises (api.DataShapeFor), so a field added to a DTO
// turns up here without anybody remembering to copy it, and a hand-written
// copy cannot drift into a lie.

// OutputSchema builds the tool's declared output schema.
func (t Tool) OutputSchema() *Schema {
	return &Schema{
		Type:        "object",
		Description: "The result envelope. `data` is this tool's own object; `next_cursor` is non-null only when there is another page; `warnings` is always present and usually empty.",
		Properties: map[string]*Schema{
			"data":        t.dataSchema(),
			"next_cursor": t.cursorSchema(),
			"warnings": {
				Type:        "array",
				Items:       &Schema{Type: "string"},
				Description: "Anything the server changed or wants the caller to know about this answer. Normalisation is never silent, so a cleaned value is reported here rather than quietly accepted.",
			},
		},
		Required:             []string{"data", "next_cursor", "warnings"},
		AdditionalProperties: boolPtr(false),
	}
}

func (t Tool) cursorSchema() *Schema {
	if !t.paginated() {
		return &Schema{
			Type:        "null",
			Description: "Always null: this tool returns one object rather than a page.",
		}
	}
	return &Schema{
		Type:        []string{"string", "null"},
		Description: "Pass this back as `cursor` to fetch the next page. Null means this was the last page.",
	}
}

// paginated reports whether the tool's route is a listing.
func (t Tool) paginated() bool {
	if t.Route == "" {
		return false
	}
	r, ok := api.RouteByName(t.Route)
	return ok && r.Paginated
}

// dataSchema is `data`, typed by the tool's DTO.
func (t Tool) dataSchema() *Schema {
	route := t.Route
	if route == "" && len(t.Routes) > 0 {
		// `remove_reaction` reaches two routes that answer the same DTO.
		route = t.Routes[0]
	}
	shape, ok := api.DataShapeFor(route)
	if !ok {
		return &Schema{Description: "This tool's object."}
	}
	body := schemaForType(shape.Type)
	if !shape.Items {
		body.Description = "This tool's object."
		return body
	}
	props := map[string]*Schema{
		"items": {
			Type:        "array",
			Items:       body,
			Description: "The rows of this page, newest first. A listing always puts its rows here and never in `data` as a bare array.",
		},
	}
	required := []string{"items"}
	if shape.Coverage {
		coverage := schemaForType(api.SearchCoverageType())
		coverage.Description = "Whether the search index is complete, so a thin answer during a backfill is visible rather than mistaken for an empty one."
		props["coverage"] = coverage
		required = append(required, "coverage")
	}
	return &Schema{
		Type:        "object",
		Description: "This tool's page of rows.",
		Properties:  props,
		Required:    required,
	}
}
