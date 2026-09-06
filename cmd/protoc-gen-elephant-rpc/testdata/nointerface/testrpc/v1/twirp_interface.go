// This file is not generated. It is a verbatim copy of the interfaces
// protoc-gen-twirp emits for testrpc/v1/service.proto, and it is what makes
// the nointerface variant of the fixture compile: with interface=false the
// plugin emits adapters that reference an interface Twirp owns. If the
// interface protoc-gen-elephant-rpc generates with interface=true ever stops
// matching this one, the withinterface variant of the fixture will still
// compile and this one will not, which is the point.

package testrpcv1

import context "context"

// ===================
// Documents Interface
// ===================

// Documents is the service that stores and retrieves documents. The comment
// is here to check that a service level doc comment ends up on the generated
// interface, the way protoc-gen-twirp puts it there.
type Documents interface {
	// Get retrieves a document version.
	Get(context.Context, *GetRequest) (*GetResponse, error)

	// Update creates a new version of a document. The comment runs over more
	// than one line so that the fixture covers a multi-line doc comment being
	// carried over verbatim.
	Update(context.Context, *UpdateRequest) (*UpdateResponse, error)

	Delete(context.Context, *DeleteRequest) (*DeleteResponse, error)
}

// =================
// Schemas Interface
// =================

// Schemas manages the document schemas.
type Schemas interface {
	// Register adds a schema version.
	Register(context.Context, *RegisterSchemaRequest) (*RegisterSchemaResponse, error)

	// List returns the registered schemas.
	List(context.Context, *ListSchemasRequest) (*ListSchemasResponse, error)
}
