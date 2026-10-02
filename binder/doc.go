// Package binder implements sectioned request binding: a request type declares
// one sub-struct per source, tagged `bind:"path"`, `bind:"query"` or
// `bind:"body"`. Sections that are absent from the type are never decoded.
//
// Path and query fields are flat: strings, bools, numbers, types implementing
// encoding.TextUnmarshaler (uuid.UUID, time.Time), pointers to those, and in
// the query slices of them, read from repeated parameters. Their tag holds
// only the name; rules such as required go in the `binding` tag. Unknown
// query parameters and JSON keys are rejected with a 400. Bodies are capped
// at 1 MiB.
//
//	type CreateUserReq struct {
//	    Path struct {
//	        OrgID int `path:"org_id" binding:"required,min=1"`
//	    } `bind:"path"`
//	    Query struct {
//	        Notify bool `query:"notify"`
//	    } `bind:"query"`
//	    Body *struct {
//	        Name  string `json:"name"  binding:"required"`
//	        Email string `json:"email" binding:"required,email"`
//	    } `bind:"body"`
//	}
//
//	b := binder.NewBinder(binder.WithErrorMapper(mapErrors))
//	mux.HandleFunc("POST /orgs/{org_id}/users", b.Bind(createUser))
//
// Handlers get the session with session.From[User](r); session.RequireUser keeps
// requests without a user from reaching them.
//
// Bodies must be JSON: any other Content-Type is answered with 415. Forms and
// multipart uploads are out of scope; read them from r in the handler.
//
// Bodies are decoded and responses encoded with encoding/json/v2, not v1:
// member names match case-sensitively, nil slices and maps encode as [] and {}
// rather than null, and omitempty follows v2's rules. Use `json:",case:ignore"`
// or the json.MatchCaseInsensitiveNames option where v1 behaviour is needed.
//
// All reflection over the request type happens inside Bind, which runs at route
// registration, and malformed request types panic there.
package binder
