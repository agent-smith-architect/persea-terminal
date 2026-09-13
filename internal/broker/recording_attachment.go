package broker

import "persea-terminal/internal/terminal"

// Epoch's release runs after its work gate, input and egress really settle.
// Caller Finalize timeouts leave this ordinary owner hold intact.
type recordingAttachmentEffects struct {
	delegate terminal.AttachmentEffects
	lease    *recordingReaderLease
}

func (effects *recordingAttachmentEffects) BindAttachment(epoch *terminal.Epoch) error {
	effects.lease.hold()
	if effects.delegate != nil {
		return effects.delegate.BindAttachment(epoch)
	}
	return nil
}

func (effects *recordingAttachmentEffects) ReleaseAttachment(epoch *terminal.Epoch) error {
	defer effects.lease.done()
	if effects.delegate != nil {
		return effects.delegate.ReleaseAttachment(epoch)
	}
	return nil
}
