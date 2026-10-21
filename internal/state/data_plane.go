package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

const DataPlaneGateway = "gateway"

type DataPlaneObservation struct {
	RunningGatewayMode string    `json:"running_gateway_mode,omitempty"`
	ObservedAt         time.Time `json:"observed_at"`
	DataPlaneObservationNeutral
}

type DataPlaneObservationNeutral struct {
	Planes  map[string]DataPlaneComponentObservation `json:"planes,omitempty"`
	Workers map[string]DataPlaneComponentObservation `json:"workers,omitempty"`
}

type DataPlaneComponentObservation struct {
	Running bool `json:"running"`
}

func (observation *DataPlaneObservation) UnmarshalJSON(body []byte) error {
	type observationAlias DataPlaneObservation
	var decoded observationAlias
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	*observation = DataPlaneObservation(decoded)
	if _, ok := observation.Planes[DataPlaneGateway]; ok {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	payload, ok := fields[legacyDataPlaneRunningField()]
	if ok {
		var running bool
		if err := json.Unmarshal(payload, &running); err != nil {
			return err
		}
		if observation.Planes == nil {
			observation.Planes = map[string]DataPlaneComponentObservation{}
		}
		observation.Planes[DataPlaneGateway] = DataPlaneComponentObservation{Running: running}
	}
	return nil
}

func legacyDataPlaneRunningField() string {
	return "sing_box_running"
}

func (s *Store) LoadDataPlaneObservation() (DataPlaneObservation, error) {
	var observation DataPlaneObservation
	err := loadJSON(s.layout.RuntimePath("data-plane-observation"), &observation)
	if errors.Is(err, os.ErrNotExist) {
		return DataPlaneObservation{}, nil
	}
	if err != nil {
		return DataPlaneObservation{}, err
	}
	return NormalizeDataPlaneObservation(observation)
}

func (s *Store) SaveDataPlaneObservation(observation DataPlaneObservation) error {
	normalized, err := NormalizeDataPlaneObservation(observation)
	if err != nil {
		return err
	}
	_, err = saveJSONAtomicIfChanged(s.layout.RuntimePath("data-plane-observation"), normalized, 0o600)
	return err
}

// DataPlaneObservationForMode builds a control-plane observation from the
// applied/gateway mode that was just made live. on/global imply the gateway
// data plane should be running; safe/off imply it should not.
func DataPlaneObservationForMode(mode string) DataPlaneObservation {
	mode = EffectiveGatewayMode(mode)
	running := mode == GatewayModeOn || mode == GatewayModeGlobal
	return DataPlaneObservationForGateway(mode, running)
}

func DataPlaneObservationForGateway(mode string, running bool) DataPlaneObservation {
	observation := DataPlaneObservation{
		RunningGatewayMode: mode,
		DataPlaneObservationNeutral: DataPlaneObservationNeutral{
			Planes: map[string]DataPlaneComponentObservation{
				DataPlaneGateway: {Running: running},
			},
		},
		ObservedAt: time.Now().UTC(),
	}
	return observation
}

func NormalizeDataPlaneObservation(observation DataPlaneObservation) (DataPlaneObservation, error) {
	if observation.Planes == nil {
		observation.Planes = map[string]DataPlaneComponentObservation{}
	}
	if _, ok := observation.Planes[DataPlaneGateway]; !ok {
		observation.Planes[DataPlaneGateway] = DataPlaneComponentObservation{}
	}
	return observation, nil
}
