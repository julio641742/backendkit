// Package binder turns plain functions into HTTP handlers, filling their
// arguments by type: the request's context, values from providers such as
// the logged in user, and one input struct decoded from the request.
//
//	type createUserInput struct {
//	    OrgID  int  `path:"org_id" binding:"required,min=1"`
//	    Notify bool `query:"notify"`
//	    Body   struct {
//	        Name  string `json:"name"  binding:"required"`
//	        Email string `json:"email" binding:"required,email"`
//	    }
//	}
//
//	func createUser(ctx context.Context, admin *User, in createUserInput) (*binder.Result, error)
//
//	b := binder.NewBinder(binder.Provide(currentUser), binder.WithErrorMapper(mapErrors))
//	mux.HandleFunc("POST /orgs/{org_id}/users", b.Bind(createUser))
//
// The input struct is flat. Each field is a path parameter (`path:"name"`),
// a query parameter (`query:"name"`), or the field named Body, the JSON body,
// which must be a struct. Embedded structs are flattened, so shared parameters
// can be reused. Parameters are strings, bools, numbers, types implementing
// encoding.TextUnmarshaler (uuid.UUID, time.Time) and pointers to those, each
// given at most once. Their tag holds only the name; rules such as required
// go in the `binding` tag. Unknown query parameters and JSON keys are
// rejected with a 400. Bodies are capped at 1 MiB.
//
// Bodies must be JSON: any other Content-Type is answered with 415. Forms and
// multipart uploads are out of scope; take *http.Request and read them from
// it in the handler.
//
// Bodies are decoded and responses encoded with encoding/json/v2, not v1:
// member names match case-sensitively, nil slices and maps encode as [] and {}
// rather than null, and omitempty follows v2's rules. Use `json:",case:ignore"`
// or the json.MatchCaseInsensitiveNames option where v1 behaviour is needed.
//
// All reflection over the handler happens inside Bind, which runs at route
// registration, and handlers with a malformed signature or input struct panic
// there. A test that builds the router catches them all.
package binder
