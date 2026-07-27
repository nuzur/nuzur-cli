package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

// GetOnlineLocalAgents returns the agents paired to this account that are
// currently online, each carrying its published connections.
//
// Offline agents are excluded: an extension run through an offline agent can
// only fail, so offering one would just produce a failed round-trip.
//
// Only the caller's own agents are listed. The web app also offers agents a
// teammate shared with the project's team, which needs the
// ListTeamAgentConnections RPC — that one isn't in protodeps/gen yet, so adding
// shared agents here means regenerating the protos first (protodeps/gen.sh).
func (i *Implementation) GetOnlineLocalAgents() ([]*nemgen.LocalAgent, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListLocalAgents(ctx, &gen.ListLocalAgentsRequest{})
	if err != nil {
		return nil, err
	}

	var online []*nemgen.LocalAgent
	for _, a := range res.GetLocalAgents() {
		if a.GetStatus() == nemgen.LocalAgentStatus_LOCAL_AGENT_STATUS_ONLINE {
			online = append(online, a)
		}
	}
	return online, nil
}
