package management

import (
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
)

// Kinds under which console jobs are shared between cluster nodes.
const (
	jobKindImageGenerationTest = "image_generation_test"
	jobKindVideoGenerationTest = "video_generation_test"
	jobKindModelTest           = "model_test"
)

type jobSnapshotStoreHolder struct{ store jobsnapshot.Store }

// sharedJobSnapshots is set in cluster mode, where the panel's polls for a
// console job may reach a node other than the one running it.
var sharedJobSnapshots atomic.Pointer[jobSnapshotStoreHolder]

// SetSharedJobSnapshots makes console jobs created from now on publish their
// progress through store; nil keeps them in the running node's memory only.
func SetSharedJobSnapshots(store jobsnapshot.Store) {
	if store == nil {
		sharedJobSnapshots.Store(nil)
		return
	}
	sharedJobSnapshots.Store(&jobSnapshotStoreHolder{store: store})
}

func sharedJobSnapshotStore() jobsnapshot.Store {
	if holder := sharedJobSnapshots.Load(); holder != nil {
		return holder.store
	}
	return nil
}

func clusterNodeID() string { return cluster.Default().NodeID() }

func shareTaskSnapshots(svc *imagegeneration.Service, kind string) *imagegeneration.Service {
	if store := sharedJobSnapshotStore(); store != nil && svc != nil {
		svc.ShareSnapshots(store, kind, clusterNodeID)
	}
	return svc
}

func shareRefreshJobs(svc *aiaccountstatus.Service) *aiaccountstatus.Service {
	if store := sharedJobSnapshotStore(); store != nil && svc != nil {
		svc.ShareJobSnapshots(store, clusterNodeID)
	}
	return svc
}
