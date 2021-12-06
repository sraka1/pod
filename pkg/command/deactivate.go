package command

import (
	"github.com/avereha/pod/pkg/response"
	log "github.com/sirupsen/logrus"
)

	type Deactivate struct {
	Seq uint8
	ID  []byte
}

func UnmarshalDeactivate(data []byte) (*Deactivate, error) {
	ret := &Deactivate{}
	// TODO deserialize this command
	log.Infof("Deactivate, 0x1c, received, data 0x%x", data)
	return ret, nil
}

func (g *Deactivate) GetResponse() (response.Response, error) {
	// TODO improve responses
	return &response.GeneralStatusResponse{}, nil
}

func (g *Deactivate) SetHeaderData(seq uint8, id []byte) error {
	g.ID = id
	g.Seq = seq
	return nil
}

func (g *Deactivate) GetHeaderData() (uint8, []byte, error) {
	return g.Seq, g.ID, nil
}