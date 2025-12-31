package querylog

import (
	"log"
	"sync"
	"time"
)

// Service provides asynchronous logging over a buffered channel.
type Service struct {
	repo    *Repository
	logChan chan *QueryLog
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewService constructs a Service with a buffered channel and background worker.
func NewService(repo *Repository) *Service {
	s := &Service{
		repo:    repo,
		logChan: make(chan *QueryLog, 1000),
		done:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.processLogs()
	return s
}

// LogAsync enqueues a log entry without blocking callers.
func (s *Service) LogAsync(entry *QueryLog) {
	select {
	case s.logChan <- entry:
	default:
		// Drop when buffer is full to avoid backpressure on request path.
		log.Println("querylog: channel full, dropping entry")
	}
}

func (s *Service) processLogs() {
	defer s.wg.Done()
	for {
		select {
		case logEntry := <-s.logChan:
			if err := s.repo.Create(logEntry); err != nil {
				log.Printf("querylog: failed to persist query log: %v", err)
			}
		case <-s.done:
			// Drain remaining logs before exiting
			for {
				select {
				case logEntry := <-s.logChan:
					if err := s.repo.Create(logEntry); err != nil {
						log.Printf("querylog: failed to persist query log during shutdown: %v", err)
					}
				default:
					return
				}
			}
		}
	}
}

// FlushAndWait signals shutdown and waits for pending logs to be written
func (s *Service) FlushAndWait(timeout time.Duration) {
	log.Println("Flushing query logs...")

	// Signal worker to stop
	close(s.done)

	// Wait with timeout
	waitCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		log.Println("Query logs flushed")
	case <-time.After(timeout):
		log.Println("Query log flush timed out")
	}
}
