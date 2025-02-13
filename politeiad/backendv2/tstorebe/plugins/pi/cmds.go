// Copyright (c) 2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package pi

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	backend "github.com/decred/politeia/politeiad/backendv2"
	"github.com/decred/politeia/politeiad/backendv2/tstorebe/store"
	"github.com/decred/politeia/politeiad/plugins/pi"
	"github.com/decred/politeia/politeiad/plugins/ticketvote"
	"github.com/decred/politeia/politeiad/plugins/usermd"
	"github.com/decred/politeia/util"
)

const (
	pluginID = pi.PluginID

	// Blob entry data descriptors
	dataDescriptorBillingStatus = pluginID + "-billingstatus-v1"
)

var (
	// billingStatusChanges contains the allowed billing status transitions. If
	// billingStatusChanges[currentStatus][newStatus] exists then the the billing
	// status transition is allowed.
	billingStatusChanges = map[pi.BillingStatusT]map[pi.BillingStatusT]struct{}{
		// Active to...
		pi.BillingStatusActive: {
			pi.BillingStatusClosed:    {},
			pi.BillingStatusCompleted: {},
		},
		// Closed to...
		pi.BillingStatusClosed: {
			pi.BillingStatusActive:    {},
			pi.BillingStatusCompleted: {},
		},
		// Completed to...
		pi.BillingStatusCompleted: {
			pi.BillingStatusActive: {},
			pi.BillingStatusClosed: {},
		},
	}
)

// cmdSetBillingStatus sets proposal's billing status.
func (p *piPlugin) cmdSetBillingStatus(token []byte, payload string) (string, error) {
	// Decode payload
	var sbs pi.SetBillingStatus
	err := json.Unmarshal([]byte(payload), &sbs)
	if err != nil {
		return "", err
	}

	// Verify token
	err = tokenMatches(token, sbs.Token)
	if err != nil {
		return "", err
	}

	// Verify billing status
	switch sbs.Status {
	case pi.BillingStatusActive, pi.BillingStatusClosed,
		pi.BillingStatusCompleted:
		// These are allowed; continue

	default:
		// Billing status is invalid
		return "", backend.PluginError{
			PluginID:     pi.PluginID,
			ErrorCode:    uint32(pi.ErrorCodeBillingStatusInvalid),
			ErrorContext: "invalid billing status",
		}
	}

	// Verify signature
	msg := sbs.Token + strconv.FormatUint(uint64(sbs.Status), 10) + sbs.Reason
	err = util.VerifySignature(sbs.Signature, sbs.PublicKey, msg)
	if err != nil {
		return "", convertSignatureError(err)
	}

	// Ensure reason is provided when status is set to closed.
	if sbs.Status == pi.BillingStatusClosed && sbs.Reason == "" {
		return "", backend.PluginError{
			PluginID:  pi.PluginID,
			ErrorCode: uint32(pi.ErrorCodeBillingStatusChangeNotAllowed),
			ErrorContext: "must provide a reason when setting " +
				"billing status to closed",
		}
	}

	// Ensure proposal's vote ended and it was approved
	vsr, err := p.voteSummary(token)
	if err != nil {
		return "", err
	}
	if vsr.Status != ticketvote.VoteStatusApproved {
		return "", backend.PluginError{
			PluginID:  pi.PluginID,
			ErrorCode: uint32(pi.ErrorCodeBillingStatusChangeNotAllowed),
			ErrorContext: "setting billing status is allowed only if " +
				"proposal vote was approved",
		}
	}

	// Ensure that this is not an RFP proposal. RFP proposals do not
	// request funding and do not bill against the treasury, which
	// means that they don't have a billing status. RFP submission
	// proposals, however, do request funding and do have a billing
	// status.
	r, err := p.record(backend.RecordRequest{
		Token:     token,
		Filenames: []string{ticketvote.FileNameVoteMetadata},
	})
	if err != nil {
		return "", err
	}
	vm, err := voteMetadataDecode(r.Files)
	if err != nil {
		return "", err
	}
	if isRFP(vm) {
		return "", backend.PluginError{
			PluginID:     pi.PluginID,
			ErrorCode:    uint32(pi.ErrorCodeBillingStatusChangeNotAllowed),
			ErrorContext: "rfp proposals do not have a billing status",
		}
	}

	// Ensure number of billing status changes does not exceed the maximum
	bscs, err := p.billingStatusChanges(token)
	if err != nil {
		return "", err
	}
	if uint32(len(bscs)+1) > p.billingStatusChangesMax {
		return "", backend.PluginError{
			PluginID:  pi.PluginID,
			ErrorCode: uint32(pi.ErrorCodeBillingStatusChangeNotAllowed),
			ErrorContext: "number of billing status changes exceeds the " +
				"maximum allowed number of billing status changes",
		}
	}

	// Ensure billing status change transition is valid
	currStatus := proposalBillingStatus(vsr.Status, bscs)
	_, ok := billingStatusChanges[currStatus][sbs.Status]
	if !ok {
		return "", backend.PluginError{
			PluginID:  pi.PluginID,
			ErrorCode: uint32(pi.ErrorCodeBillingStatusChangeNotAllowed),
			ErrorContext: fmt.Sprintf("invalid billing status transition, "+
				"%v to %v is not allowed", pi.BillingStatuses[currStatus],
				pi.BillingStatuses[sbs.Status]),
		}
	}

	// Save billing status change
	receipt := p.identity.SignMessage([]byte(sbs.Signature))
	bsc := pi.BillingStatusChange{
		Token:     sbs.Token,
		Status:    sbs.Status,
		Reason:    sbs.Reason,
		PublicKey: sbs.PublicKey,
		Signature: sbs.Signature,
		Timestamp: time.Now().Unix(),
		Receipt:   hex.EncodeToString(receipt[:]),
	}
	err = p.billingStatusSave(token, bsc)
	if err != nil {
		return "", err
	}

	// Prepare reply
	sbsr := pi.SetBillingStatusReply{
		Timestamp: bsc.Timestamp,
		Receipt:   bsc.Receipt,
	}
	reply, err := json.Marshal(sbsr)
	if err != nil {
		return "", err
	}

	return string(reply), nil
}

// tokenMatches verifies that the command token (the token for the record that
// this plugin command is being executed on) matches the payload token (the
// token that the plugin command payload contains that is typically used in the
// payload signature). The payload token must be the full length token.
func tokenMatches(cmdToken []byte, payloadToken string) error {
	pt, err := tokenDecode(payloadToken)
	if err != nil {
		return backend.PluginError{
			PluginID:     pi.PluginID,
			ErrorCode:    uint32(pi.ErrorCodeTokenInvalid),
			ErrorContext: util.TokenRegexp(),
		}
	}
	if !bytes.Equal(cmdToken, pt) {
		return backend.PluginError{
			PluginID:  pi.PluginID,
			ErrorCode: uint32(pi.ErrorCodeTokenInvalid),
			ErrorContext: fmt.Sprintf("payload token does not "+
				"match command token: got %x, want %x",
				pt, cmdToken),
		}
	}
	return nil
}

// cmdBillingStatusChanges returns the billing status changes of a proposal.
func (p *piPlugin) cmdBillingStatusChanges(token []byte) (string, error) {
	// Get billing status changes
	bscs, err := p.billingStatusChanges(token)
	if err != nil {
		return "", err
	}

	// Prepare reply
	bscsr := pi.BillingStatusChangesReply{
		BillingStatusChanges: bscs,
	}
	reply, err := json.Marshal(bscsr)
	if err != nil {
		return "", err
	}

	return string(reply), nil
}

// cmdSummary returns the pi summary of a proposal.
func (p *piPlugin) cmdSummary(token []byte) (string, error) {
	// Get the proposal status
	propStatus, err := p.getProposalStatus(token)
	if err != nil {
		return "", err
	}

	// Prepare the reply
	sr := pi.SummaryReply{
		Summary: pi.ProposalSummary{
			Status: propStatus,
		},
	}

	reply, err := json.Marshal(sr)
	if err != nil {
		return "", err
	}

	return string(reply), nil
}

// statusChangesDecode decodes and returns the StatusChangeMetadata from the
// metadata streams if one is present.
func statusChangesDecode(metadata []backend.MetadataStream) ([]usermd.StatusChangeMetadata, error) {
	statuses := make([]usermd.StatusChangeMetadata, 0, 16)
	for _, v := range metadata {
		if v.PluginID != usermd.PluginID ||
			v.StreamID != usermd.StreamIDStatusChanges {
			// Not the mdstream we're looking for
			continue
		}
		d := json.NewDecoder(strings.NewReader(v.Payload))
		for {
			var sc usermd.StatusChangeMetadata
			err := d.Decode(&sc)
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, err
			}
			statuses = append(statuses, sc)
		}
		break
	}
	return statuses, nil
}

// proposalBillingStatus accepts proposal's vote status with the billing status
// changes and returns the proposal's billing status.
func proposalBillingStatus(vs ticketvote.VoteStatusT, bscs []pi.BillingStatusChange) pi.BillingStatusT {
	// If proposal vote wasn't approved,
	// return invalid billing status.
	if vs != ticketvote.VoteStatusApproved {
		return pi.BillingStatusInvalid
	}

	var bs pi.BillingStatusT
	if len(bscs) == 0 {
		// Proposals that have been approved, but have not had
		// their billing status set yet are considered to be
		// active.
		bs = pi.BillingStatusActive
	} else {
		// Use the status from the most recent billing status
		// change.
		bs = bscs[len(bscs)-1].Status
	}

	return bs
}

// record returns a record from the backend with it's contents filtered
// according to the provided record request.
//
// A backend ErrRecordNotFound error is returned if the record is not found.
func (p *piPlugin) record(rr backend.RecordRequest) (*backend.Record, error) {
	if rr.Token == nil {
		return nil, errors.Errorf("token not provided")
	}
	reply, err := p.backend.Records([]backend.RecordRequest{rr})
	if err != nil {
		return nil, err
	}
	r, ok := reply[hex.EncodeToString(rr.Token)]
	if !ok {
		return nil, backend.ErrRecordNotFound
	}
	return &r, nil
}

// recordAbridged returns a record with all files omitted.
//
// A backend ErrRecordNotFound error is returned if the record is not found.
func (p *piPlugin) recordAbridged(token []byte) (*backend.Record, error) {
	rr := backend.RecordRequest{
		Token:        token,
		OmitAllFiles: true,
	}
	return p.record(rr)
}

// convertSignatureError converts a util SignatureError to a backend
// PluginError that contains a pi plugin error code.
func convertSignatureError(err error) backend.PluginError {
	var e util.SignatureError
	var s pi.ErrorCodeT
	if errors.As(err, &e) {
		switch e.ErrorCode {
		case util.ErrorStatusPublicKeyInvalid:
			s = pi.ErrorCodePublicKeyInvalid
		case util.ErrorStatusSignatureInvalid:
			s = pi.ErrorCodeSignatureInvalid
		}
	}
	return backend.PluginError{
		PluginID:     pi.PluginID,
		ErrorCode:    uint32(s),
		ErrorContext: e.ErrorContext,
	}
}

// billingStatusSave saves a BillingStatusChange to the backend.
func (p *piPlugin) billingStatusSave(token []byte, bsc pi.BillingStatusChange) error {
	// Prepare blob
	be, err := billingStatusEncode(bsc)
	if err != nil {
		return err
	}

	// Save blob
	return p.tstore.BlobSave(token, *be)
}

// billingStatusChanges returns the billing status changes of a proposal.
func (p *piPlugin) billingStatusChanges(token []byte) ([]pi.BillingStatusChange, error) {
	// Retrieve blobs
	blobs, err := p.tstore.BlobsByDataDesc(token,
		[]string{dataDescriptorBillingStatus})
	if err != nil {
		return nil, err
	}

	// Decode blobs
	statusChanges := make([]pi.BillingStatusChange, 0, len(blobs))
	for _, v := range blobs {
		a, err := billingStatusDecode(v)
		if err != nil {
			return nil, err
		}
		statusChanges = append(statusChanges, *a)
	}

	// Sanity check. They should already be sorted from oldest to
	// newest.
	sort.SliceStable(statusChanges, func(i, j int) bool {
		return statusChanges[i].Timestamp < statusChanges[j].Timestamp
	})

	return statusChanges, nil
}

// billingStatusEncode encodes a BillingStatusChange into a BlobEntry.
func billingStatusEncode(bsc pi.BillingStatusChange) (*store.BlobEntry, error) {
	data, err := json.Marshal(bsc)
	if err != nil {
		return nil, err
	}
	hint, err := json.Marshal(
		store.DataDescriptor{
			Type:       store.DataTypeStructure,
			Descriptor: dataDescriptorBillingStatus,
		})
	if err != nil {
		return nil, err
	}
	be := store.NewBlobEntry(hint, data)
	return &be, nil
}

// billingStatusDecode decodes a BlobEntry into a BillingStatusChange.
func billingStatusDecode(be store.BlobEntry) (*pi.BillingStatusChange, error) {
	// Decode and validate data hint
	b, err := base64.StdEncoding.DecodeString(be.DataHint)
	if err != nil {
		return nil, fmt.Errorf("decode DataHint: %v", err)
	}
	var dd store.DataDescriptor
	err = json.Unmarshal(b, &dd)
	if err != nil {
		return nil, fmt.Errorf("unmarshal DataHint: %v", err)
	}
	if dd.Descriptor != dataDescriptorBillingStatus {
		return nil, fmt.Errorf("unexpected data descriptor: got %v, "+
			"want %v", dd.Descriptor, dataDescriptorBillingStatus)
	}

	// Decode data
	b, err = base64.StdEncoding.DecodeString(be.Data)
	if err != nil {
		return nil, fmt.Errorf("decode Data: %v", err)
	}
	digest, err := hex.DecodeString(be.Digest)
	if err != nil {
		return nil, fmt.Errorf("decode digest: %v", err)
	}
	if !bytes.Equal(util.Digest(b), digest) {
		return nil, fmt.Errorf("data is not coherent; got %x, want %x",
			util.Digest(b), digest)
	}
	var bsc pi.BillingStatusChange
	err = json.Unmarshal(b, &bsc)
	if err != nil {
		return nil, fmt.Errorf("unmarshal AuthDetails: %v", err)
	}

	return &bsc, nil
}
