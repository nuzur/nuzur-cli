package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

// GetProjectVersion returns a project version WITH its schema — entities,
// fields, enums and relationships.
//
// ListProjectVersions is the cheaper listing and carries none of that, so
// anything that has to look inside the model (resolving an entity and field by
// identifier, for instance) has to come through here.
func (i *Implementation) GetProjectVersion(projectVersionUUID string) (*nemgen.ProjectVersion, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	return i.productClient.ProductClient.GetProjectVersionForUser(ctx, &gen.GetProjectVersionForUserRequest{
		ProjectVersionUuid: projectVersionUUID,
	})
}

// GetStandaloneEntities returns all standalone entities from a project version.
func (i *Implementation) GetStandaloneEntities(projectVersionUUID string) ([]*nemgen.Entity, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	pv, err := i.productClient.ProductClient.GetProjectVersionForUser(ctx, &gen.GetProjectVersionForUserRequest{
		ProjectVersionUuid: projectVersionUUID,
	})
	if err != nil {
		return nil, err
	}

	var result []*nemgen.Entity
	for _, e := range pv.Entities {
		if e.Type == nemgen.EntityType_ENTITY_TYPE_STANDALONE {
			result = append(result, e)
		}
	}
	return result, nil
}

// GetTeamConnections returns the active connections of the team that owns the
// given project — the same list the web's extension config form offers.
func (i *Implementation) GetTeamConnections(projectTeamUUID string) ([]*nemgen.Connection, error) {
	team, err := i.getTeam(projectTeamUUID)
	if err != nil {
		return nil, err
	}
	return activeConnections(team), nil
}

// GetTeamDBStores returns the active database stores (team settings →
// databases) of the team that owns the given project. A DB_STORE field means
// one of these, never an object store: sql-push and sql-import pick a store,
// then a connection that belongs to it.
func (i *Implementation) GetTeamDBStores(projectTeamUUID string) ([]*nemgen.Store, error) {
	team, err := i.getTeam(projectTeamUUID)
	if err != nil {
		return nil, err
	}
	return activeDBStores(team), nil
}

func (i *Implementation) getTeam(teamUUID string) (*nemgen.Team, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}
	return i.productClient.ProductClient.GetTeamForUser(ctx, &gen.GetTeamForUserRequest{TeamUuid: teamUUID})
}

func activeDBStores(team *nemgen.Team) []*nemgen.Store {
	var out []*nemgen.Store
	for _, s := range team.GetStores() {
		if s.GetStatus() == nemgen.StoreStatus_STORE_STATUS_ACTIVE {
			out = append(out, s)
		}
	}
	return out
}

func activeConnections(team *nemgen.Team) []*nemgen.Connection {
	var out []*nemgen.Connection
	for _, c := range team.GetConnections() {
		if c.GetStatus() == nemgen.ConnectionStatus_CONNECTION_STATUS_ACTIVE {
			out = append(out, c)
		}
	}
	return out
}
