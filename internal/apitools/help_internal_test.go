package apitools

import (
	"errors"
	"testing"
)

// helpContract is a contract whose fields reach their descriptions every
// way the contract writes them: a parameter by reference, a property of
// the request body, of an array's items, through allOf and oneOf, by
// reference without a description of its own, under a response, and a
// self-reference deeper than a field is looked for.
const helpContract = `x-problem-codes: [request.invalid_body]
paths:
  /v1/things:
    parameters:
      - name: ignored
        in: query
    post:
      operationId: makeThing
      summary: Make a thing
      description: Makes a thing.
      x-problem-codes: [things.too_big]
      parameters:
        - $ref: "#/components/parameters/Mode"
      requestBody:
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Thing"}
      responses:
        "400": {description: Refused.}
        "201":
          content:
            application/json:
              schema:
                type: object
                properties:
                  id: {type: string, description: The thing's id.}
components:
  parameters:
    Mode: {name: mode, in: query, description: How it is made.}
  schemas:
    Money:
      type: object
      description: An amount and its currency.
      properties:
        amount: {type: string, description: The amount.}
    Thing:
      allOf:
        - type: object
          properties:
            price: {$ref: "#/components/schemas/Money"}
        - oneOf:
            - type: object
              properties:
                lines:
                  type: array
                  items:
                    type: object
                    properties:
                      quantity: {type: string, description: How many.}
                deep: {$ref: "#/components/schemas/Deep"}
    Deep:
      type: object
      properties:
        next: {$ref: "#/components/schemas/Deep"}
`

// TestExplainFields: a field is found as a parameter, a request body's
// property (nested, in an array's items, through combined schemas, its
// description that of the schema it refers to where it has none) or a
// successful response's; a field nowhere, one deeper than the lookup
// goes, an unknown operation and a problem code the operation does not
// list are not in the contract; a problem code every operation may answer
// is.
func TestExplainFields(t *testing.T) {
	c, err := load([]byte(helpContract))
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"mode":           "How it is made.",
		"price":          "An amount and its currency.",
		"price.amount":   "The amount.",
		"lines.quantity": "How many.",
		"id":             "The thing's id.",
	} {
		t.Run(field, func(t *testing.T) {
			e, err := c.explain("makeThing", field, "")
			if err != nil || e.FieldDescription != want {
				t.Errorf("got %q (%v), want %q", e.FieldDescription, err, want)
			}
		})
	}
	for name, q := range map[string][3]string{
		"a field nowhere":        {"makeThing", "colour", ""},
		"a field too deep":       {"makeThing", "deep.next.next.next.next.next.next.next.next.next.next.next.next.next", ""},
		"a field under a scalar": {"makeThing", "price.amount.digits", ""},
		"an unknown operation":   {"breakThing", "", ""},
		"no operation":           {"", "", ""},
		"a problem not listed":   {"makeThing", "", "things.too_small"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.explain(q[0], q[1], q[2]); !errors.Is(err, ErrNotInContract) {
				t.Errorf("got %v, want ErrNotInContract", err)
			}
		})
	}
	for _, problem := range []string{"things.too_big", "request.invalid_body"} {
		if e, err := c.explain("makeThing", "", problem); err != nil || e.Problem != problem || e.Operation.Summary != "Make a thing" {
			t.Errorf("%s: got %+v (%v), want the operation and the code", problem, e, err)
		}
	}
}
