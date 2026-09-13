package proto

// RefitFailureStage is a closed, non-sensitive diagnostic for a refit that
// crossed the tmux width-mutation point of no return. It is safe to carry
// across the broker/front boundary and to write to service logs; arbitrary
// error text remains private to the engine.
type RefitFailureStage string

const (
	RefitFailureCapture          RefitFailureStage = "capture"
	RefitFailureBindSuccessor    RefitFailureStage = "bind_successor"
	RefitFailureMaterialize      RefitFailureStage = "materialize_successor"
	RefitFailureBeginRegistry    RefitFailureStage = "begin_registry"
	RefitFailureQueueBootstrap   RefitFailureStage = "queue_bootstrap"
	RefitFailureSubmitBoundary   RefitFailureStage = "submit_boundary"
	RefitFailureAwaitBoundary    RefitFailureStage = "await_boundary"
	RefitFailureValidateRegistry RefitFailureStage = "validate_registry"
	RefitFailureSubmitSeal       RefitFailureStage = "submit_seal"
	RefitFailureAwaitSeal        RefitFailureStage = "await_seal"
	RefitFailureCommitRegistry   RefitFailureStage = "commit_registry"
	RefitFailureCommitJournal    RefitFailureStage = "commit_journal"
	RefitFailureQueuePending     RefitFailureStage = "queue_pending"
	RefitFailureSubmitPending    RefitFailureStage = "submit_pending"
	RefitFailureAwaitPending     RefitFailureStage = "await_pending"
)

func IsRefitFailureStage(stage RefitFailureStage) bool {
	switch stage {
	case RefitFailureCapture, RefitFailureBindSuccessor, RefitFailureMaterialize,
		RefitFailureBeginRegistry, RefitFailureQueueBootstrap, RefitFailureSubmitBoundary, RefitFailureAwaitBoundary,
		RefitFailureValidateRegistry, RefitFailureSubmitSeal, RefitFailureAwaitSeal,
		RefitFailureCommitRegistry, RefitFailureCommitJournal,
		RefitFailureQueuePending, RefitFailureSubmitPending, RefitFailureAwaitPending:
		return true
	default:
		return false
	}
}

// RefitFailureClass is the closed public class paired with a failure stage.
// It deliberately excludes paths, operation tokens, payloads and error text.
type RefitFailureClass string

const (
	RefitFailureInvalidated RefitFailureClass = "invalidated"
	RefitFailureCapacity    RefitFailureClass = "capacity"
	RefitFailureStorage     RefitFailureClass = "storage"
	RefitFailureObserver    RefitFailureClass = "observer"
	RefitFailureInternal    RefitFailureClass = "internal"
)

func IsRefitFailureClass(class RefitFailureClass) bool {
	switch class {
	case RefitFailureInvalidated, RefitFailureCapacity, RefitFailureStorage, RefitFailureObserver, RefitFailureInternal:
		return true
	default:
		return false
	}
}
