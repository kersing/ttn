package broker

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	pb "github.com/TheThingsNetwork/ttn/api/broker"
	"github.com/TheThingsNetwork/ttn/api/gateway"
	"github.com/TheThingsNetwork/ttn/api/networkserver"
	pb_networkserver "github.com/TheThingsNetwork/ttn/api/networkserver"
	"github.com/TheThingsNetwork/ttn/core/types"
	"github.com/brocaar/lorawan"
)

const maxFCntGap = 16384

var (
	ErrNotFound    = errors.New("ttn/broker: Device not found")
	ErrNoMatch     = errors.New("ttn/broker: No matching device")
	ErrInvalidFCnt = errors.New("ttn/broker: Invalid Frame Counter")
)

func (b *broker) HandleUplink(uplink *pb.UplinkMessage) error {
	time := time.Now()

	// De-duplicate uplink messages
	duplicates := b.deduplicateUplink(uplink)
	if len(duplicates) == 0 {
		return nil
	}

	base := duplicates[0]

	if base.ProtocolMetadata.GetLorawan() == nil {
		return errors.New("ttn/broker: Can not handle uplink from non-LoRaWAN device")
	}

	// LoRaWAN: Unmarshal
	var phyPayload lorawan.PHYPayload
	err := phyPayload.UnmarshalBinary(base.Payload)
	if err != nil {
		return err
	}
	macPayload, ok := phyPayload.MACPayload.(*lorawan.MACPayload)
	if !ok {
		return errors.New("Uplink message does not contain a MAC payload.")
	}

	// Request devices from NS
	devAddr := types.DevAddr(macPayload.FHDR.DevAddr)
	getDevicesResp, err := b.ns.GetDevices(b.Component.GetContext(), &networkserver.DevicesRequest{
		DevAddr: &devAddr,
		FCnt:    macPayload.FHDR.FCnt,
	})
	if err != nil {
		return err
	}
	if len(getDevicesResp.Results) == 0 {
		return ErrNotFound
	}

	// Find AppEUI/DevEUI through MIC check
	var device *pb_networkserver.DevicesResponse_Device
	for _, candidate := range getDevicesResp.Results {
		nwkSKey := lorawan.AES128Key(*candidate.NwkSKey)
		if candidate.Uses32BitFCnt {
			macPayload.FHDR.FCnt = candidate.FullFCnt
		}
		ok, err = phyPayload.ValidateMIC(nwkSKey)
		if err != nil {
			return err
		}
		if ok {
			device = candidate
			break
		}
	}
	if device == nil {
		return ErrNoMatch
	}

	if device.DisableFCntCheck {
		// TODO: Add warning to message?
	} else if macPayload.FHDR.FCnt < device.StoredFCnt || macPayload.FHDR.FCnt-device.StoredFCnt > maxFCntGap {
		// Replay attack or FCnt gap too big
		return ErrInvalidFCnt
	}

	// Add FCnt to Metadata (because it's not marshaled in lorawan payload)
	base.ProtocolMetadata.GetLorawan().FCnt = device.FullFCnt

	// Collect GatewayMetadata and DownlinkOptions
	var gatewayMetadata []*gateway.RxMetadata
	var downlinkOptions []*pb.DownlinkOption
	var downlinkMessage *pb.DownlinkMessage
	for _, duplicate := range duplicates {
		gatewayMetadata = append(gatewayMetadata, duplicate.GatewayMetadata)
		downlinkOptions = append(downlinkOptions, duplicate.DownlinkOptions...)
	}

	// Select best DownlinkOption
	if len(downlinkOptions) > 0 {
		downlinkMessage = &pb.DownlinkMessage{
			DownlinkOption: selectBestDownlink(downlinkOptions),
		}
	}

	// Build Uplink
	deduplicatedUplink := &pb.DeduplicatedUplinkMessage{
		Payload:          base.Payload,
		DevEui:           device.DevEui,
		AppEui:           device.AppEui,
		ProtocolMetadata: base.ProtocolMetadata,
		GatewayMetadata:  gatewayMetadata,
		ServerTime:       time.UnixNano(),
		ResponseTemplate: downlinkMessage,
	}

	// Pass Uplink through NS
	deduplicatedUplink, err = b.ns.Uplink(b.Component.GetContext(), deduplicatedUplink)
	if err != nil {
		return err
	}

	application, err := b.applications.Get(*device.AppEui)
	if err != nil {
		return err
	}

	handler, err := b.getHandler(application.HandlerID)
	if err != nil {
		return err
	}

	handler <- deduplicatedUplink

	return nil
}

func (b *broker) deduplicateUplink(duplicate *pb.UplinkMessage) (uplinks []*pb.UplinkMessage) {
	sum := md5.Sum(duplicate.Payload)
	key := hex.EncodeToString(sum[:])
	list := b.uplinkDeduplicator.Deduplicate(key, duplicate)
	if len(list) == 0 {
		return
	}
	for _, duplicate := range list {
		uplinks = append(uplinks, duplicate.(*pb.UplinkMessage))
	}
	return
}

func selectBestDownlink(options []*pb.DownlinkOption) *pb.DownlinkOption {
	sort.Sort(ByScore(options))
	return options[0]
}
