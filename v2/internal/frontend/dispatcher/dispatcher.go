package dispatcher

import (
	"github.com/pkg/errors"
	"github.com/wailsapp/wails/v2/internal/binding"
	"github.com/wailsapp/wails/v2/internal/frontend"
	"github.com/wailsapp/wails/v2/internal/logger"
)

type Dispatcher struct {
	log      *logger.Logger
	bindings *binding.Bindings
	events   frontend.Events
}

func NewDispatcher(log *logger.Logger, bindings *binding.Bindings, events frontend.Events) *Dispatcher {
	return &Dispatcher{
		log:      log,
		bindings: bindings,
		events:   events,
	}
}

func (d *Dispatcher) ProcessMessage(message string) error {
	if message == "" {
		return errors.New("No message to process")
	}
	switch message[0] {
	case 'L':
		return d.processLogMessage(message)
	case 'E':
		return d.processEventMessage(message)
	default:
		return errors.New("Unknown message from front end: " + message)
	}
}
