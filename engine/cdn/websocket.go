package cdn

import (
	"context"
	"encoding/json"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/websocket"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/telemetry"
)

const wbBrokerPubSubKey = "cdn_ws_broker_pubsub"

func (s *Service) initWebsocket() error {
	log.Info(s.Router.Background, "Initializing WS server")
	s.WSServer = &websocketServer{
		server:     websocket.NewServer(),
		clientData: make(map[string]*websocketClientData),
	}

	log.Info(s.Router.Background, "Initializing WS events broker")
	pubSub, err := s.Cache.Subscribe(wbBrokerPubSubKey)
	if err != nil {
		return sdk.WrapError(err, "unable to subscribe to %s", wbBrokerPubSubKey)
	}
	s.WSBroker = websocket.NewBroker()
	s.WSBroker.OnMessage(func(m []byte) {
		telemetry.Record(s.Router.Background, s.Metrics.WSEvents, 1)
		var e sdk.CDNWSEvent
		if err := sdk.JSONUnmarshal(m, &e); err != nil {
			err = sdk.WrapError(err, "cannot parse event from WS broker")
			ctx := sdk.ContextWithStacktrace(context.TODO(), err)
			log.Warn(ctx, err.Error())
			return
		}

		s.websocketOnMessage(e)
	})
	s.WSBroker.Init(s.Router.Background, s.GoRoutines, pubSub)

	s.GoRoutines.RunWithRestart(s.Router.Background, "cdn.initWebsocket.SendWSEvents", func(ctx context.Context) {
		tickerMetrics := time.NewTicker(10 * time.Second)
		defer tickerMetrics.Stop()
		tickerPublish := time.NewTicker(100 * time.Millisecond)
		defer tickerPublish.Stop()

		for {
			select {
			case <-ctx.Done():
				telemetry.Record(s.Router.Background, s.Metrics.WSClients, 0)
				return
			case <-tickerMetrics.C:
				telemetry.Record(s.Router.Background, s.Metrics.WSClients, int64(len(s.WSServer.server.ClientIDs())))
			case <-tickerPublish.C:
				if err := s.sendWSEvent(ctx); err != nil {
					ctx = sdk.ContextWithStacktrace(ctx, err)
					log.Error(ctx, err.Error())
				}
			}
		}
	})
	return nil
}

func (s *Service) publishWSEvent(itemUnit sdk.CDNItemUnit) {
	s.WSEventsMutex.Lock()
	defer s.WSEventsMutex.Unlock()
	if s.WSEvents == nil {
		s.WSEvents = make(map[string]sdk.CDNWSEvent)
	}
	var event sdk.CDNWSEvent
	apiRefItem, _ := itemUnit.Item.GetCDNLogApiRef()
	if apiRefItem != nil {
		event = sdk.CDNWSEvent{
			ItemType: itemUnit.Type,
			JobRunID: strconv.FormatInt(apiRefItem.NodeRunJobID, 10),
		}
	} else {
		apiRefLogItem, _ := itemUnit.Item.GetCDNLogApiRefV2()
		if apiRefLogItem == nil {
			return
		}
		event = sdk.CDNWSEvent{
			ItemType: itemUnit.Type,
			JobRunID: apiRefLogItem.RunJobID,
		}
	}

	event.ItemUnitID = itemUnit.ID
	s.WSEvents[itemUnit.ID] = event
}

func (s *Service) sendWSEvent(ctx context.Context) error {
	s.WSEventsMutex.Lock()
	es := make([]sdk.CDNWSEvent, 0, len(s.WSEvents))
	for _, v := range s.WSEvents {
		es = append(es, v)
	}
	s.WSEvents = nil
	s.WSEventsMutex.Unlock()

	for _, e := range es {
		buf, err := json.Marshal(e)
		if err != nil {
			return sdk.WithStack(err)
		}
		if err := s.Cache.Publish(ctx, wbBrokerPubSubKey, string(buf)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) websocketOnMessage(e sdk.CDNWSEvent) {
	// Randomize the order of client to prevent the old client to always received new events in priority
	clientIDs := s.WSServer.server.ClientIDs()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(clientIDs), func(i, j int) { clientIDs[i], clientIDs[j] = clientIDs[j], clientIDs[i] })

	for _, id := range clientIDs {
		c := s.WSServer.GetClientData(id)

		if c == nil || c.itemFilter == nil || c.itemFilter.JobRunID != e.JobRunID {
			continue
		}

		// Add new step on client data
		c.mutexData.Lock()
		if _, has := c.itemUnitsData[e.ItemUnitID]; !has {
			c.itemUnitsData[e.ItemUnitID] = ItemUnitClientData{}
		}
		c.mutexData.Unlock()
		c.TriggerUpdate()
	}
}

type websocketServer struct {
	server     *websocket.Server
	mutex      sync.RWMutex
	clientData map[string]*websocketClientData
}

func (s *websocketServer) AddClient(c websocket.Client, data *websocketClientData) {
	s.server.AddClient(c)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.clientData[c.UUID()] = data
}

func (s *websocketServer) RemoveClient(uuid string) {
	s.server.RemoveClient(uuid)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.clientData, uuid)
}

func (s *websocketServer) GetClientData(uuid string) *websocketClientData {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	data, ok := s.clientData[uuid]
	if !ok {
		return nil
	}
	return data
}

type websocketClientData struct {
	sessionID       string
	mutexData       sync.Mutex
	itemFilter      *sdk.CDNStreamFilter
	itemUnitsData   map[string]ItemUnitClientData
	mutexTrigger    sync.Mutex
	triggeredUpdate bool
}

type ItemUnitClientData struct {
	itemUnit            *sdk.CDNItemUnit
	scoreNextLineToSend int64
}

func (d *websocketClientData) TriggerUpdate() {
	d.mutexTrigger.Lock()
	defer d.mutexTrigger.Unlock()
	d.triggeredUpdate = true
}

func (d *websocketClientData) ConsumeTrigger() (triggered bool) {
	d.mutexTrigger.Lock()
	defer d.mutexTrigger.Unlock()
	triggered = d.triggeredUpdate
	d.triggeredUpdate = false
	return
}

func (d *websocketClientData) UpdateFilter(filter sdk.CDNStreamFilter, itemUnitID string) error {
	if err := filter.Validate(); err != nil {
		return err
	}

	d.mutexData.Lock()
	defer d.mutexData.Unlock()

	d.itemFilter = &filter
	if d.itemUnitsData == nil || d.itemFilter.JobRunID != filter.JobRunID {
		d.itemUnitsData = make(map[string]ItemUnitClientData)
	}
	if itemUnitID != "" {
		if _, ok := d.itemUnitsData[itemUnitID]; !ok {
			d.itemUnitsData[itemUnitID] = ItemUnitClientData{
				itemUnit:            nil,
				scoreNextLineToSend: -10,
			}
		}
	}
	return nil
}
