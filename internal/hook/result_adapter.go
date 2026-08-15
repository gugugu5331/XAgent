package hook

import (
	"errors"

	"xagent/internal/tool"
)

var errSyntheticResultAdapter = errors.New("hook synthetic result adapter requires a result factory")

// SyntheticResultAdapter constructs bounded results caused by Hook decisions.
// It deliberately retains only ResultFactory: Hook denial/error paths are
// synthetic and must never receive Capture, Store, Writer, or path capability.
type SyntheticResultAdapter struct {
	resultFactory *tool.ResultFactory
}

func NewSyntheticResultAdapter(resultFactory *tool.ResultFactory) (*SyntheticResultAdapter, error) {
	if resultFactory == nil {
		return nil, errSyntheticResultAdapter
	}
	return &SyntheticResultAdapter{resultFactory: resultFactory}, nil
}

func (adapter *SyntheticResultAdapter) Build(input tool.ResultFactoryInput) (tool.Result, error) {
	if adapter == nil || adapter.resultFactory == nil {
		return tool.Result{}, errSyntheticResultAdapter
	}
	return adapter.resultFactory.Build(input)
}

// ToolOutputFromUserView adapts a projected user view for Hook dispatch. It
// cannot observe ModelContent, PersistedContent, OutputMeta, or tool.Result.
func ToolOutputFromUserView(view tool.UserView) ToolOutput {
	content := view.Preview
	if content.Text() == "" {
		content = view.Summary
	}
	output := ToolOutput{
		Status:  hookStatusFromUserView(view),
		Content: content,
	}
	if view.Error != nil {
		output.Error = &SafeError{
			Code:        view.Error.Code,
			Message:     view.Error.Message,
			Recoverable: view.Error.Recoverable,
		}
	}
	return output
}

func hookStatusFromUserView(view tool.UserView) ToolStatus {
	if view.Status == tool.StatusTimeout || view.State == tool.CancelledAfterStart {
		return ToolTimeout
	}
	if view.Status == tool.StatusSuccess {
		return ToolSuccess
	}
	return ToolErrorStatus
}
