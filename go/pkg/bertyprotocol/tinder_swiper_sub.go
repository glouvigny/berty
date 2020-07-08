package bertyprotocol

import (
	"context"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"
)

type Swiper struct {
	muWatch  sync.RWMutex
	muTopics sync.Mutex
	topics   map[string]*pubsub.Topic

	interval time.Duration

	logger *zap.Logger
	pubsub *pubsub.PubSub
}

func NewSwiper(logger *zap.Logger, ps *pubsub.PubSub, interval time.Duration) *Swiper {
	return &Swiper{
		logger:   logger,
		pubsub:   ps,
		topics:   make(map[string]*pubsub.Topic),
		interval: interval,
	}
}

func (s *Swiper) topicJoin(topic string, opts ...pubsub.TopicOpt) (*pubsub.Topic, error) {
	s.muTopics.Lock()
	defer s.muTopics.Unlock()

	var err error

	t, ok := s.topics[topic]
	if ok {
		return t, nil
	}

	if t, err = s.pubsub.Join(topic, opts...); err != nil {
		return nil, err
	}

	if _, err = t.Relay(); err != nil {
		t.Close()
		return nil, err
	}

	s.topics[topic] = t
	return t, nil
}

func (s *Swiper) topicLeave(topic string) (err error) {
	s.muTopics.Lock()
	if t, ok := s.topics[topic]; ok {
		t.Relay()
		err = t.Close()
		delete(s.topics, topic)
	}
	s.muTopics.Unlock()
	return
}

// watchUntilDeadline looks for peers providing a resource for a given period
func (s *Swiper) watchUntilDeadline(ctx context.Context, out chan<- peer.AddrInfo, topic string, end time.Time) error {
	s.logger.Debug("start watching", zap.String("topic", topic))
	tp, err := s.topicJoin(topic)
	if err != nil {
		return err
	}

	for _, p := range tp.ListPeers() {
		s.logger.Debug("peer joined topic",
			zap.String("topic", topic),
			zap.String("peer", p.ShortString()),
		)

		out <- peer.AddrInfo{
			ID: p,
		}

	}

	ctx, cancel := context.WithDeadline(ctx, end)

	eventHandler := func(te *pubsub.TopicEventHandler) error {
		defer s.topicLeave(topic)
		defer cancel()

		s.logger.Debug("start watch event handler")
		for {
			pe, err := te.NextPeerEvent(ctx)
			if err != nil {
				s.logger.Debug("next peer event error", zap.Error(err))
				return err
			}

			s.logger.Debug("event received")
			switch pe.Type {
			case pubsub.PeerJoin:
				s.logger.Debug("peer joined topic",
					zap.String("topic", topic),
					zap.String("peer", pe.Peer.ShortString()),
				)
				out <- peer.AddrInfo{
					ID: pe.Peer,
				}
			case pubsub.PeerLeave:
			}
		}
	}

	_, err = tp.EventHandler(eventHandler)
	if err != nil {
		return err
	}

	return nil
}

// watch looks for peers providing a resource
func (s *Swiper) WatchTopic(ctx context.Context, topic, seed []byte) chan peer.AddrInfo {
	out := make(chan peer.AddrInfo)
	ctx, cancel := context.WithCancel(ctx)
	s.logger.Debug("start watch topic")
	go func() {
		defer cancel()
		defer close(out)

		for {
			roundedTime := roundTimePeriod(time.Now(), s.interval)
			topicForTime := generateRendezvousPointForPeriod(topic, seed, roundedTime)
			periodEnd := nextTimePeriod(roundedTime, s.interval)
			err := s.watchUntilDeadline(ctx, out, string(topicForTime), periodEnd)
			if err != nil {
				s.logger.Error("failed to start watcher", zap.Error(err))
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(periodEnd)):
			}
		}
	}()

	return out
}

// watch looks for peers providing a resource
func (s *Swiper) Announce(ctx context.Context, topic, seed []byte) {
	ctx, cancel := context.WithCancel(ctx)
	var currentTopic string

	s.logger.Debug("start watch announce")
	go func() {
		defer cancel()
		for {
			if currentTopic != "" {
				if err := s.topicLeave(currentTopic); err != nil {
					s.logger.Warn("failed to start close current topic", zap.Error(err))
				}
			}

			roundedTime := roundTimePeriod(time.Now(), s.interval)
			currentTopic = string(generateRendezvousPointForPeriod(topic, seed, roundedTime))
			_, err := s.topicJoin(currentTopic)
			if err != nil {
				s.logger.Error("failed to announce topic", zap.Error(err))
				return
			}

			periodEnd := nextTimePeriod(roundedTime, s.interval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(periodEnd)):
			}
		}
	}()
}

// func (s *Swiper) Announce(ctx context.Context, topic, seed []byte) error {

// 	ctx, cancel := context.WithCancel(ctx)
// 	mu := &sync.RWMutex{}
// 	mu.Lock()

// 	var topicForTime string
// 	var tic *time.Timer
// 	tic = time.AfterFunc(0, func() {
// 		mu.RLock()
// 		defer mu.RUnlock()

// 		roundedTime := roundTimePeriod(time.Now(), s.interval)
// 		topicForTime := string(generateRendezvousPointForPeriod(topic, seed, roundedTime))

// 		err := s.topicClose(topicForTime)
// 		t, err := s.topicJoin(topicForTime)

// 		if err != nil {
// 			s.logger.Error("failed to start watcher", zap.Error(err))
// 			cancel()
// 			return
// 		}

// 		if !tic.Stop() {
// 			<-tic.C
// 		}

// 		periodEnd := nextTimePeriod(roundedTime, s.interval)
// 		tic.Reset(time.Until(periodEnd))
// 	})

// 	go func() {
// 		<-ctx.Done()
// 		mu.Lock()
// 		if !tic.Stop() {
// 			<-tic.C
// 		}

// 		close(out)
// 		mu.Unlock()
// 	}()

// 	mu.Unlock()

// }

// // announceForPeriod advertise a topic for a given period, will either stop when the context is done or when the period has ended
// func (s *Swiper) announceForPeriod(ctx context.Context, topic, seed []byte, t time.Time) (<-chan bool, <-chan error) {
// 	announces := make(chan bool)
// 	errs := make(chan error)

// 	roundedTime := roundTimePeriod(t, s.interval)
// 	topicForTime := generateRendezvousPointForPeriod(topic, seed, roundedTime)

// 	nextStart := nextTimePeriod(roundedTime, s.interval)

// 	go func() {
// 		ctx, cancel := context.WithDeadline(ctx, nextStart)
// 		defer cancel()

// 		defer close(errs)
// 		defer close(announces)
// 		defer func() { errs <- io.EOF }()

// 		for {
// 			duration, err := s.tinder.Advertise(ctx, string(topicForTime))
// 			if err != nil {
// 				errs <- err
// 				return
// 			}

// 			announces <- true

// 			if ctx.Err() != nil || time.Now().Add(duration).UnixNano() > nextStart.UnixNano() {
// 				return
// 			}
// 		}
// 	}()

// 	return announces, errs
// }

// // announce advertises availability on a topic indefinitely
// func (s *Swiper) announce(ctx context.Context, topic, seed []byte) (<-chan bool, <-chan error) {
// 	announces := make(chan bool)
// 	errs := make(chan error)

// 	go func() {
// 		defer close(announces)
// 		defer close(errs)
// 		defer func() { errs <- io.EOF }()

// 		for {
// 			select {
// 			case <-ctx.Done():
// 				return
// 			default:
// 				periodAnnounces, periodErrs := s.announceForPeriod(ctx, topic, seed, time.Now())

// 				select {
// 				case announce := <-periodAnnounces:
// 					announces <- announce
// 					break

// 				case err := <-periodErrs:
// 					errs <- err
// 					break
// 				}
// 			}
// 		}
// 	}()

// 	return announces, errs
// }
