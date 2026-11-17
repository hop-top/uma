package sms

import (
	"context"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// stageMedia stages the media of an incoming MMS through the sink and adds
// an attachment part per staged item to in's content, next to the
// PayloadMedia summary; see twilio.Client.StageMedia for what is staged and
// when it fails. It reports only a failure worth a redelivery; media that
// cannot be staged for good, or a sink that stages nothing, leaves the
// message with its summary only; for a download that failed for good the
// summary names the media unavailable (Media.Unavailable).
func (i *Instance) stageMedia(ctx context.Context, sink adapter.Sink, form url.Values, in *model.Ingress) error {
	stager, ok := sink.(adapter.AttachmentSink)
	if !ok || i.rest == nil || in.Envelope.Kind != model.EnvelopeMessage {
		return nil
	}
	var gaps []twilio.MediaGap
	refs, err := i.rest.StageMedia(ctx, stager, twilio.MediaRequest{
		AccountSID: i.cfg.AccountSID, MessageSID: in.Envelope.MessageID,
		Destination: in.Envelope.Destination, Audience: in.Envelope.Audience, Form: form,
		Skipped: func(g twilio.MediaGap) { gaps = append(gaps, g) },
		Credential: func(ctx context.Context) (ports.Credential, error) {
			return i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
		},
	})
	for k := range refs {
		in.Envelope.Content.Parts = append(in.Envelope.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &refs[k]})
	}
	reportGaps(in, gaps)
	return err
}

// reportGaps lists the media that failed for good in the message's media
// summary, so the loss is visible to whoever reads the message.
func reportGaps(in *model.Ingress, gaps []twilio.MediaGap) {
	if len(gaps) == 0 {
		return
	}
	for k, p := range in.Envelope.Content.Parts {
		if p.Structured == nil || p.Structured.Name != PayloadMedia {
			continue
		}
		if data, err := twilio.AnnotateMediaGaps(p.Structured.Data, gaps); err == nil {
			ext := *p.Structured
			ext.Data = data
			in.Envelope.Content.Parts[k].Structured = &ext
		}
		return
	}
}
