package provider

import (
	"errors"

	"xagent/internal/budget"
	"xagent/internal/config"
)

const maxStreamInitialAllocation int64 = 64 << 10

// streamLimits owns every budget for one provider stream. Counters are kept
// separate because several independently accumulated limits use budget.Bytes.
type streamLimits struct {
	response      *scopedStreamCounter
	event         *scopedStreamCounter
	events        *scopedStreamCounter
	text          *scopedStreamCounter
	thinking      *scopedStreamCounter
	toolArguments *scopedStreamCounter
}

type scopedStreamCounter struct {
	scope     budget.Scope
	dimension budget.Dimension
	counter   *budget.Counter
}

func newStreamLimits(cfg config.StreamConfig) (*streamLimits, error) {
	response, err := newScopedStreamCounter(budget.ProviderMaxResponseBytes, cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	event, err := newScopedStreamCounter(budget.ProviderMaxEventBytes, cfg.MaxEventBytes)
	if err != nil {
		return nil, err
	}
	events, err := newScopedStreamCounter(budget.ProviderMaxEvents, cfg.MaxEvents)
	if err != nil {
		return nil, err
	}
	text, err := newScopedStreamCounter(budget.ProviderMaxTextBytes, cfg.MaxTextBytes)
	if err != nil {
		return nil, err
	}
	thinking, err := newScopedStreamCounter(budget.ProviderMaxThinkingBytes, cfg.MaxThinkingBytes)
	if err != nil {
		return nil, err
	}
	toolArguments, err := newScopedStreamCounter(budget.ProviderMaxToolArgumentsBytes, cfg.MaxToolArgumentsBytes)
	if err != nil {
		return nil, err
	}

	return &streamLimits{
		response:      response,
		event:         event,
		events:        events,
		text:          text,
		thinking:      thinking,
		toolArguments: toolArguments,
	}, nil
}

func newScopedStreamCounter(scope budget.Scope, configured int64) (*scopedStreamCounter, error) {
	spec, ok := streamBudgetSpec(scope)
	if !ok {
		return nil, errors.New("provider stream budget scope is unavailable")
	}

	var candidate *int64
	if configured != 0 {
		candidate = &configured
	}
	effectiveValue, err := spec.Resolve(candidate)
	if err != nil {
		return nil, err
	}
	effective, err := budget.NewLimits(budget.Limit{Dimension: spec.Dimension, Value: effectiveValue})
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(budget.Limit{Dimension: spec.Dimension, Value: spec.HardCap})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(effective, hard)
	if err != nil {
		return nil, remapStreamLimitError(scope, err)
	}
	return &scopedStreamCounter{scope: scope, dimension: spec.Dimension, counter: counter}, nil
}

func streamBudgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func (l *streamLimits) consumeResponse(amount int64) error {
	return l.consume(l.response, amount)
}

// consumeEvent validates one encoded event independently. MaxEventBytes is a
// per-event ceiling, so successful checks must not reduce the next event's
// allowance.
func (l *streamLimits) consumeEvent(amount int64) error {
	if l == nil || l.event == nil {
		return errors.New("provider stream limits are nil")
	}
	return l.event.check(amount)
}

func (l *streamLimits) consumeEventCount() error {
	return l.consume(l.events, 1)
}

func (l *streamLimits) consumeText(amount int64) error {
	return l.consume(l.text, amount)
}

func (l *streamLimits) consumeThinking(amount int64) error {
	return l.consume(l.thinking, amount)
}

func (l *streamLimits) consumeToolArguments(amount int64) error {
	return l.consume(l.toolArguments, amount)
}

func (l *streamLimits) consume(counter *scopedStreamCounter, amount int64) error {
	if l == nil || counter == nil {
		return errors.New("provider stream limits are nil")
	}
	return counter.consume(amount)
}

func (c *scopedStreamCounter) consume(amount int64) error {
	if c == nil || c.counter == nil {
		return errors.New("provider stream budget counter is nil")
	}
	return remapStreamLimitError(c.scope, c.counter.Consume(c.dimension, amount))
}

func (c *scopedStreamCounter) check(amount int64) error {
	if c == nil || c.counter == nil {
		return errors.New("provider stream budget counter is nil")
	}
	if amount < 0 {
		return errors.New("budget consumption must not be negative")
	}
	limit := c.counter.Remaining(c.dimension)
	if amount > limit {
		return &budget.LimitError{
			Scope:     string(c.scope),
			Dimension: c.dimension,
			Limit:     limit,
			Observed:  amount,
		}
	}
	return nil
}

func remapStreamLimitError(scope budget.Scope, err error) error {
	if err == nil {
		return nil
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return err
	}
	return &budget.LimitError{
		Scope:     string(scope),
		Dimension: limitErr.Dimension,
		Limit:     limitErr.Limit,
		Observed:  limitErr.Observed,
	}
}

func (l *streamLimits) responsePreallocation(requested int64) int {
	return safeStreamPreallocation(requested, l.remaining(l.response))
}

func (l *streamLimits) eventPreallocation(requested int64) int {
	return safeStreamPreallocation(requested, l.remaining(l.event))
}

func (l *streamLimits) textPreallocation(requested int64) int {
	return safeStreamPreallocation(requested, l.remaining(l.text))
}

func (l *streamLimits) thinkingPreallocation(requested int64) int {
	return safeStreamPreallocation(requested, l.remaining(l.thinking))
}

func (l *streamLimits) toolArgumentsPreallocation(requested int64) int {
	return safeStreamPreallocation(requested, l.remaining(l.toolArguments))
}

func (l *streamLimits) remaining(counter *scopedStreamCounter) int64 {
	if l == nil || counter == nil || counter.counter == nil {
		return 0
	}
	return counter.counter.Remaining(counter.dimension)
}

func safeStreamPreallocation(requested, remaining int64) int {
	if requested <= 0 || remaining <= 0 {
		return 0
	}
	capacity := min(requested, remaining, maxStreamInitialAllocation)
	platformMaxInt := int64(^uint(0) >> 1)
	if capacity > platformMaxInt {
		capacity = platformMaxInt
	}
	return int(capacity)
}
