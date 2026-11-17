package engine_test

import (
	"context"

	"hop.top/uma/adapter"
)

var (
	_ adapter.ReceiptSender     = fullInstance{}
	_ adapter.MembershipChanger = fullInstance{}
	_ adapter.HistoryReader     = fullInstance{}
	_ adapter.RawReader         = fullInstance{}
	_ adapter.Archiver          = fullInstance{}
	_ adapter.Mover             = fullInstance{}
	_ adapter.ReadStateSetter   = fullInstance{}
	_ adapter.Flagger           = fullInstance{}
	_ adapter.Trasher           = fullInstance{}
)

func (fullInstance) History(context.Context, adapter.HistoryRequest) (adapter.HistoryPage, error) {
	return adapter.HistoryPage{}, nil
}

func (fullInstance) ReadRaw(context.Context, adapter.RawRequest) (adapter.RawMessage, error) {
	return adapter.RawMessage{}, nil
}

func (fullInstance) ChangeMembership(context.Context, adapter.MembershipRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) SendReceipt(context.Context, adapter.ReceiptRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) Archive(context.Context, adapter.ArchiveRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) Move(context.Context, adapter.MoveRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) SetReadState(context.Context, adapter.ReadStateRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) Flag(context.Context, adapter.FlagRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

func (fullInstance) Trash(context.Context, adapter.TrashRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
