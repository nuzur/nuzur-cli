package extensionrun

import (
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

func TestActiveDBStoresAreTeamStoresNotObjectStores(t *testing.T) {
	team := &nemgen.Team{
		Stores: []*nemgen.Store{
			{Uuid: "db-1", Identifier: "nuzur", Status: nemgen.StoreStatus_STORE_STATUS_ACTIVE},
			{Uuid: "db-2", Identifier: "old", Status: nemgen.StoreStatus_STORE_STATUS_DISABLED},
		},
		ObjectStores: []*nemgen.ObjectStore{{Uuid: "obj-1", Identifier: "gpgdata"}},
	}
	got := activeDBStores(team)
	if len(got) != 1 || got[0].Uuid != "db-1" {
		t.Errorf("activeDBStores = %v, want only the active DB store (never object stores)", got)
	}
}

func TestActiveConnections(t *testing.T) {
	team := &nemgen.Team{Connections: []*nemgen.Connection{
		{Uuid: "c-1", Status: nemgen.ConnectionStatus_CONNECTION_STATUS_ACTIVE},
		{Uuid: "c-2", Status: nemgen.ConnectionStatus_CONNECTION_STATUS_DISABLED},
	}}
	got := activeConnections(team)
	if len(got) != 1 || got[0].Uuid != "c-1" {
		t.Errorf("activeConnections = %v", got)
	}
}
