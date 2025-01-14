package pod

import (
	"time"

	"github.com/avereha/pod/pkg/bluetooth"
	"github.com/avereha/pod/pkg/command"
	"github.com/avereha/pod/pkg/eap"
	"github.com/avereha/pod/pkg/pair"

	"github.com/avereha/pod/pkg/encrypt"
	"github.com/avereha/pod/pkg/response"

	"github.com/davecgh/go-spew/spew"
	log "github.com/sirupsen/logrus"
)

type PodMsgBody struct {
	// This contains the decrytped message body
	//   MsgBodyCommand: incoming after stripping off address and crc
	//   MsgBodyResponse: outgoing before adding address and crc
	//      not sure how to get this to this level and don't really need it
	//   DeactivateFlag: set to true once 0x1c input is detected
	MsgBodyCommand  []byte
	// MsgBodyResponse []byte
	DeactivateFlag	bool
}

type Pod struct {
	ble   *bluetooth.Ble
	state *PODState
}

func New(ble *bluetooth.Ble, stateFile string, freshState bool) *Pod {
	var err error

	state := &PODState{
		filename: stateFile,
	}
	if !freshState {
		state, err = NewState(stateFile)
		if err != nil {
			log.Fatalf("pkg pod; could not restore pod state from %s: %+v", stateFile, err)
		}
	}

	ret := &Pod{
		ble:   ble,
		state: state,
	}

	return ret
}

func (p *Pod) StartAcceptingCommands() {
	log.Infof("pkg pod; got a new BLE connection")
	firstCmd, _ := p.ble.ReadCmd()
	log.Infof("pkg pod; got first command: as string: %s", firstCmd)

	p.ble.StartMessageLoop()

	if p.state.LTK != nil { // paired, just establish new session
		p.EapAka()
	} else {
		p.StartActivation() // not paired, get the LTK
	}
}

func (p *Pod) StartActivation() {

	pair := &pair.Pair{}
	msg, _ := p.ble.ReadMessage()
	if err := pair.ParseSP1SP2(msg); err != nil {
		log.Fatalf("pkg pod;  pkg pod; error parsing SP1SP2 %s", err)
	}
	// read PDM public key and nonce
	msg, _ = p.ble.ReadMessage()
	if err := pair.ParseSPS1(msg); err != nil {
		log.Fatalf("pkg pod; error parsing SPS1 %s", err)
	}

	msg, err := pair.GenerateSPS1()
	if err != nil {
		log.Fatal(err)
	}
	// send POD public key and nonce
	p.ble.WriteMessage(msg)

	// read PDM conf value
	msg, _ = p.ble.ReadMessage()
	pair.ParseSPS2(msg)

	// send POD conf value
	msg, err = pair.GenerateSPS2()
	if err != nil {
		log.Fatal(err)
	}
	p.ble.WriteMessage(msg)

	// receive SP0GP0 constant from PDM
	msg, _ = p.ble.ReadMessage()
	err = pair.ParseSP0GP0(msg)
	if err != nil {
		log.Fatalf("pkg pod; could not parse SP0GP0: %s", err)
	}

	// send P0 constant
	msg, err = pair.GenerateP0()
	if err != nil {
		log.Fatal(err)
	}
	p.ble.WriteMessage(msg)

	p.state.LTK, err = pair.LTK()
	if err != nil {
		log.Fatalf("pkg pod; could not get LTK %s", err)
	}
	log.Infof("pkg pod; LTK %x", p.state.LTK)
	p.state.EapAkaSeq = 1
	p.state.Save()

	p.EapAka()
}

func (p *Pod) EapAka() {

	session := eap.NewEapAkaChallenge(p.state.LTK, p.state.EapAkaSeq)

	msg, _ := p.ble.ReadMessage()
	err := session.ParseChallenge(msg)
	if err != nil {
		log.Fatalf("pkg pod; error parsing the EAP-AKA challenge: %s", err)
	}

	msg, err = session.GenerateChallengeResponse()
	if err != nil {
		log.Fatalf("pkg pod; error generating the eap-aka challenge response")
	}
	p.ble.WriteMessage(msg)

	msg, _ = p.ble.ReadMessage()
	log.Debugf("pkg pod; success? %x", msg.Payload) // TODO: figure out how error looks like
	err = session.ParseSuccess(msg)
	if err != nil {
		log.Fatalf("pkg pod; error parsing the EAP-AKA Success packet: %s", err)
	}
	p.state.CK, p.state.NoncePrefix = session.CKNoncePrefix()

	p.state.NonceSeq = 1
	p.state.MsgSeq = 1
	p.state.EapAkaSeq = session.Sqn
	log.Infof("pkg pod; got CK: %x", p.state.CK)
	log.Infof("pkg pod; got NONCE: %x", p.state.NoncePrefix)
	log.Infof("pkg pod; using NONCE SEQ: %d", p.state.NonceSeq)
	log.Infof("pkg pod; EAP-AKA session SQN: %d", p.state.EapAkaSeq)

	err = p.state.Save()
	if err != nil {
		log.Fatalf("pkg pod; Could not save the pod state: %s", err)
	}

	// initialize pMsg
	var pMsg PodMsgBody
	pMsg.MsgBodyCommand = make([]byte, 16)
	pMsg.DeactivateFlag = false
	log.Tracef("pkd pod; pMsg initialized: %+v", pMsg)

	p.CommandLoop(pMsg)
}

func (p *Pod) CommandLoop(pMsg PodMsgBody) {
	var lastMsgSeq uint8 = 0
	var data []byte = make([]byte, 4)
	var n int = 0
	for {
		if (pMsg.DeactivateFlag) {
			log.Infof("pkg pod; Pod was deactivated. Use -fresh for new pod")
			time.Sleep(1 * time.Second)
			log.Exit(0)
		}
		log.Infof("pkg pod;   *** Waiting for the next command ***")
		msg, _ := p.ble.ReadMessage()
		log.Tracef("pkg pod; got command message: %s", spew.Sdump(msg))

		if msg.SequenceNumber == lastMsgSeq {
			// this is a retry because we did not answer yet
			// ignore duplicate commands/mesages
			continue
		}
		lastMsgSeq = msg.SequenceNumber

		decrypted, err := encrypt.DecryptMessage(p.state.CK, p.state.NoncePrefix, p.state.NonceSeq, msg)
		if err != nil {
			log.Fatalf("pkg pod; could not decrypt message: %s", err)
		}
		p.state.NonceSeq++

		cmd, err := command.Unmarshal(decrypted.Payload)
		if err != nil {
			log.Fatalf("pkg pod; could not unmarshal command: %s", err)
		}
		cmdSeq, requestID, err := cmd.GetHeaderData()
		if err != nil {
			log.Fatalf("pkg pod; could not get command header data: %s", err)
		}
		p.state.CmdSeq = cmdSeq

		log.Debugf("pkd pod; cmd: %x", decrypted.Payload)
		data = decrypted.Payload
		n = len(data)
		log.Debugf("pkg pod; len = %d", n)
		if (n<16) {
			log.Fatalf("pkg pod; decrypted. Payload too short")
		}
		pMsg.MsgBodyCommand = data[13 : n-5]
		if data[13]==0x1c {
			pMsg.DeactivateFlag = true
		}
		log.Tracef("pkg pod; command pod message body = %x", pMsg.MsgBodyCommand)

		rsp, err := cmd.GetResponse()
		if err != nil {
			log.Fatalf("pkg pod; could not get command response: %s", err)
		}

		p.state.MsgSeq++
		p.state.CmdSeq++
		p.state.Save()
		responseMetadata := &response.ResponseMetadata{
			Dst:       msg.Source,
			Src:       msg.Destination,
			CmdSeq:    p.state.CmdSeq,
			MsgSeq:    p.state.MsgSeq,
			RequestID: requestID,
			AckSeq:    msg.SequenceNumber + 1,
		}
		msg, err = response.Marshal(rsp, responseMetadata)
		if err != nil {
			log.Fatalf("pkg pod; could not marshal command response: %s", err)
		}
		msg, err = encrypt.EncryptMessage(p.state.CK, p.state.NoncePrefix, p.state.NonceSeq, msg)
		if err != nil {
			log.Fatalf("pkg pod; could not encrypt response: %s", err)
		}
		p.state.NonceSeq++
		p.state.Save()

		log.Tracef("pkg pod; sending response: %s", spew.Sdump(msg))
		p.ble.WriteMessage(msg)

		log.Debugf("pkg pod; reading response ACK. Nonce seq %d", p.state.NonceSeq)
		msg, _ = p.ble.ReadMessage()
		// TODO check for SEQ numbers here and the Ack flag
		decrypted, err = encrypt.DecryptMessage(p.state.CK, p.state.NoncePrefix, p.state.NonceSeq, msg)
		if err != nil {
			log.Fatalf("pkg pod; could not decrypt message: %s", err)
		}
		p.state.NonceSeq++
		if len(decrypted.Payload) != 0 {
			log.Fatalf("pkg pod; this should be empty message with ACK header %s", spew.Sdump(msg))
		}
		p.state.Save()
	}
}
