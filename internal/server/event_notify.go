package server

import iota "github.com/unimpl/Iota"

// Notifications wake readers; the durable log remains the only event source.
// A slow reader gets one pending wakeup and never blocks an SDK callback.
func (s *Server) subscribeEvents(path string) chan struct{} {
	wake := make(chan struct{}, 1)
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.subscribers == nil {
		s.subscribers = make(map[string]map[chan struct{}]struct{})
	}
	if s.subscribers[path] == nil {
		s.subscribers[path] = make(map[chan struct{}]struct{})
	}
	s.subscribers[path][wake] = struct{}{}
	return wake
}

func (s *Server) unsubscribeEvents(path string, wake chan struct{}) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	delete(s.subscribers[path], wake)
	if len(s.subscribers[path]) == 0 {
		delete(s.subscribers, path)
	}
}

func (s *Server) notifyEvents(path string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	for wake := range s.subscribers[path] {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (owned *workspace) publish(event iota.Event) {
	if immediateEvent(string(event.Type)) {
		owned.notify()
	}
}
