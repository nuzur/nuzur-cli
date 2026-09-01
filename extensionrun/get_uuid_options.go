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

// GetTeamConnections returns all connections for the team that owns the given project.
func (i *Implementation) GetTeamConnections(projectTeamUUID string) ([]*nemgen.Connection, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	team, err := i.productClient.ProductClient.GetTeamForUser(ctx, &gen.GetTeamForUserRequest{
		TeamUuid: projectTeamUUID,
	})
	if err != nil {
		return nil, err
	}
	return team.Connections, nil
}

// GetTeamObjectStores returns all object stores for the team that owns the given project.
func (i *Implementation) GetTeamObjectStores(projectTeamUUID string) ([]*nemgen.ObjectStore, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	team, err := i.productClient.ProductClient.GetTeamForUser(ctx, &gen.GetTeamForUserRequest{
		TeamUuid: projectTeamUUID,
	})
	if err != nil {
		return nil, err
	}
	return team.ObjectStores, nil
}
