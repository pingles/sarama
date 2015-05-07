package sarama

import (
	"sync"
	"time"
)

// Offset Manager

// OffsetManager uses Kafka to store and fetch consumed partition offsets.
type OffsetManager interface {
	// ManagePartition creates a PartitionOffsetManager on the given topic/partition. It will
	// return an error if this OffsetManager is already managing the given topic/partition.
	ManagePartition(topic string, partition int32) (PartitionOffsetManager, error)
}

type offsetManager struct {
	client Client
	conf   *Config
	group  string

	lock sync.Mutex
	poms map[string]map[int32]*partitionOffsetManager
	boms map[*Broker]*brokerOffsetManager
}

// NewOffsetManagerFromClient creates a new OffsetManager from the given client.
// It is still necessary to call Close() on the underlying client when finished with the partition manager.
func NewOffsetManagerFromClient(client Client) (OffsetManager, error) {
	// Check that we are not dealing with a closed Client before processing any other arguments
	if client.Closed() {
		return nil, ErrClosedClient
	}

	om := &offsetManager{
		client: client,
		conf:   client.Config(),
		poms:   make(map[string]map[int32]*partitionOffsetManager),
		boms:   make(map[*Broker]*brokerOffsetManager),
	}

	return om, nil
}

func (om *offsetManager) ManagePartition(topic string, partition int32) (PartitionOffsetManager, error) {
	pom, err := om.newPartitionOffsetManager(topic, partition)
	if err != nil {
		return nil, err
	}

	om.lock.Lock()
	defer om.lock.Unlock()

	topicManagers := om.poms[topic]
	if topicManagers == nil {
		topicManagers = make(map[int32]*partitionOffsetManager)
		om.poms[topic] = topicManagers
	}

	if topicManagers[partition] != nil {
		return nil, ConfigurationError("That topic/partition is already being managed")
	}

	topicManagers[partition] = pom
	return pom, nil
}

func (om *offsetManager) refBrokerOffsetManager(broker *Broker) *brokerOffsetManager {
	om.lock.Lock()
	defer om.lock.Unlock()

	bom := om.boms[broker]
	if bom == nil {
		bom = om.newBrokerOffsetManager(broker)
		om.boms[broker] = bom
	}

	bom.refs++

	return bom
}

func (om *offsetManager) unrefBrokerOffsetManager(bom *brokerOffsetManager) {
	om.lock.Lock()
	defer om.lock.Unlock()

	bom.refs--

	if bom.refs == 0 {
		close(bom.newSubscriptions)
		if om.boms[bom.broker] == bom {
			delete(om.boms, bom.broker)
		}
	}
}

func (om *offsetManager) abandonBroker(bom *brokerOffsetManager) {
	om.lock.Lock()
	defer om.lock.Unlock()

	delete(om.boms, bom.broker)
}

// Partition Offset Manager

// PartitionOffsetManager uses Kafka to store and fetch consumed partition offsets. You MUST call Close()
// on a partition offset manager to avoid leaks, it will not be garbage-collected automatically when it passes
// out of scope.
type PartitionOffsetManager interface {
	// TODO these need documentation
	Offset() int64
	SetOffset(offset int64)

	Metadata() string
	SetMetadata(metadata string)

	Errors() <-chan error
	AsyncClose()
	Close() error
}

type partitionOffsetManager struct {
	parent    *offsetManager
	topic     string
	partition int32

	lock     sync.Mutex
	offset   int64
	metadata string
	broker   *brokerOffsetManager

	errors    chan error
	rebalance chan none
}

func (om *offsetManager) newPartitionOffsetManager(topic string, partition int32) (*partitionOffsetManager, error) {
	pom := &partitionOffsetManager{
		parent:    om,
		topic:     topic,
		partition: partition,
		errors:    make(chan error, om.conf.ChannelBufferSize),
		rebalance: make(chan none, 1),
	}

	if err := pom.selectBroker(); err != nil {
		return nil, err
	}

	if err := pom.fetchInitialOffset(); err != nil {
		return nil, err
	}

	go withRecover(pom.mainLoop)

	return pom, nil
}

func (pom *partitionOffsetManager) mainLoop() {
	for _ = range pom.rebalance {
		if err := pom.selectBroker(); err != nil {
			pom.handleError(err)
			pom.rebalance <- none{}
		}
	}
}

func (pom *partitionOffsetManager) selectBroker() error {
	if pom.broker != nil {
		pom.parent.unrefBrokerOffsetManager(pom.broker)
		pom.broker = nil
	}

	var broker *Broker
	var err error

	if err = pom.parent.client.RefreshCoordinator(pom.parent.group); err != nil {
		return err
	}

	if broker, err = pom.parent.client.Coordinator(pom.parent.group); err != nil {
		return err
	}

	pom.broker = pom.parent.refBrokerOffsetManager(broker)
	pom.broker.newSubscriptions <- pom
	return nil
}

func (pom *partitionOffsetManager) fetchInitialOffset() error {
	request := new(OffsetFetchRequest)
	request.Version = 1 // TODO should the version be configurable?
	request.ConsumerGroup = pom.parent.group
	request.AddPartition(pom.topic, pom.partition)

	response, err := pom.broker.broker.FetchOffset(request)
	if err != nil {
		return err
	}

	block := response.GetBlock(pom.topic, pom.partition)
	if block == nil {
		return ErrIncompleteResponse
	}

	switch block.Err {
	case ErrNoError:
		pom.offset = block.Offset
		pom.metadata = block.Metadata
		return nil
	default:
		// TODO what other errors can occur here, should we retry some of them?
		return err
	}
}

func (pom *partitionOffsetManager) handleError(err error) {
	// TODO should OffsetManager have its own section of `Config` or should it just borrow Consumer's?
	if pom.parent.conf.Consumer.Return.Errors {
		pom.errors <- err
	} else {
		Logger.Println(err)
	}
}

func (pom *partitionOffsetManager) Errors() <-chan error {
	return pom.errors
}

func (pom *partitionOffsetManager) SetOffset(offset int64) {
	pom.lock.Lock()
	defer pom.lock.Unlock()

	pom.offset = offset
}

func (pom *partitionOffsetManager) SetMetadata(metadata string) {
	pom.lock.Lock()
	defer pom.lock.Unlock()

	pom.metadata = metadata
}

func (pom *partitionOffsetManager) Offset() int64 {
	pom.lock.Lock()
	defer pom.lock.Unlock()

	return pom.offset
}

func (pom *partitionOffsetManager) Metadata() string {
	pom.lock.Lock()
	defer pom.lock.Unlock()

	return pom.metadata
}

func (pom *partitionOffsetManager) AsyncClose() {
	// TODO implement me
}

func (pom *partitionOffsetManager) Close() error {
	pom.AsyncClose()
	// TODO read from errors (do we need another error type a la ConsumerError(s)?)
	return nil
}

// Broker Offset Manager

type brokerOffsetManager struct {
	parent           *offsetManager
	broker           *Broker
	timer            *time.Ticker
	newSubscriptions chan *partitionOffsetManager
	subscriptions    map[*partitionOffsetManager]none
	refs             int
}

func (om *offsetManager) newBrokerOffsetManager(broker *Broker) *brokerOffsetManager {
	bom := &brokerOffsetManager{
		parent:           om,
		broker:           broker,
		timer:            time.NewTicker(5 * time.Second), // TODO this should be configurable
		newSubscriptions: make(chan *partitionOffsetManager),
		subscriptions:    make(map[*partitionOffsetManager]none),
	}

	go withRecover(bom.mainLoop)

	return bom
}

func (bom *brokerOffsetManager) mainLoop() {
	for {
		select {
		case <-bom.timer.C:
			bom.flushToBroker()
		case s, ok := <-bom.newSubscriptions:
			if !ok {
				bom.timer.Stop()
				return
			}
			bom.subscriptions[s] = none{}
		}
	}
}

func (bom *brokerOffsetManager) flushToBroker() {
	request := bom.constructRequest()
	response, err := bom.broker.CommitOffset(request)

	if err != nil {
		bom.abort(err)
	}

	for s := range bom.subscriptions {
		var err KError
		var ok bool

		if response.Errors[s.topic] == nil {
			s.handleError(ErrIncompleteResponse)
			delete(bom.subscriptions, s)
			s.rebalance <- none{}
			continue
		}
		if err, ok = response.Errors[s.topic][s.partition]; !ok {
			s.handleError(ErrIncompleteResponse)
			delete(bom.subscriptions, s)
			s.rebalance <- none{}
			continue
		}

		switch err {
		case ErrNoError:
			break
		case ErrUnknownTopicOrPartition, ErrNotLeaderForPartition, ErrLeaderNotAvailable:
			delete(bom.subscriptions, s)
			s.rebalance <- none{}
		default:
			s.handleError(err)
			delete(bom.subscriptions, s)
			s.rebalance <- none{}
		}
	}
}

func (bom *brokerOffsetManager) constructRequest() *OffsetCommitRequest {
	r := &OffsetCommitRequest{} // TODO use the right version
	for s := range bom.subscriptions {
		s.lock.Lock()
		r.AddBlock(s.topic, s.partition, s.offset, 0, s.metadata)
		s.lock.Unlock()
	}
	return r
}

func (bom *brokerOffsetManager) abort(err error) {
	bom.parent.abandonBroker(bom)
	_ = bom.broker.Close() // we don't care about the error this might return, we already have one

	for pom := range bom.subscriptions {
		pom.handleError(err)
		pom.rebalance <- none{}
	}

	for s := range bom.newSubscriptions {
		s.handleError(err)
		s.rebalance <- none{}
	}
}