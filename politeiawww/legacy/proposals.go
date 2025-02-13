// Copyright (c) 2017-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package legacy

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/decred/politeia/decredplugin"
	pdv2 "github.com/decred/politeia/politeiad/api/v2"
	piplugin "github.com/decred/politeia/politeiad/plugins/pi"
	"github.com/decred/politeia/politeiad/plugins/ticketvote"
	tkplugin "github.com/decred/politeia/politeiad/plugins/ticketvote"
	"github.com/decred/politeia/politeiad/plugins/usermd"
	umplugin "github.com/decred/politeia/politeiad/plugins/usermd"
	rcv1 "github.com/decred/politeia/politeiawww/api/records/v1"
	www "github.com/decred/politeia/politeiawww/api/www/v1"
	"github.com/decred/politeia/politeiawww/legacy/user"
	"github.com/decred/politeia/util"
	"github.com/google/uuid"
)

func (p *Politeiawww) proposals(ctx context.Context, reqs []pdv2.RecordRequest) (map[string]www.ProposalRecord, error) {
	// Break the requests up so that they do not exceed the politeiad
	// records page size.
	var startIdx int
	proposals := make(map[string]www.ProposalRecord, len(reqs))
	for startIdx < len(reqs) {
		// Setup a page of requests
		endIdx := startIdx + int(pdv2.RecordsPageSize)
		if endIdx > len(reqs) {
			endIdx = len(reqs)
		}

		page := reqs[startIdx:endIdx]
		records, err := p.politeiad.Records(ctx, page)
		if err != nil {
			return nil, err
		}

		// Get records' comment counts
		tokens := make([]string, 0, len(page))
		for _, r := range page {
			tokens = append(tokens, r.Token)
		}
		counts, err := p.politeiad.CommentCount(ctx, tokens)
		if err != nil {
			return nil, err
		}

		for k, v := range records {
			// Legacy www routes are only for vetted records
			if v.State == pdv2.RecordStateUnvetted {
				continue
			}

			// Convert to a proposal
			pr, err := convertRecordToProposal(v)
			if err != nil {
				return nil, err
			}

			count := counts[k]
			pr.NumComments = uint(count)

			// Get submissions list if this is an RFP
			if pr.LinkBy != 0 {
				subs, err := p.politeiad.TicketVoteSubmissions(ctx,
					pr.CensorshipRecord.Token)
				if err != nil {
					return nil, err
				}
				pr.LinkedFrom = subs
			}

			// Fill in user data
			userID := userIDFromMetadataStreams(v.Metadata)
			uid, err := uuid.Parse(userID)
			if err != nil {
				return nil, err
			}
			u, err := p.db.UserGetById(uid)
			if err != nil {
				return nil, err
			}
			pr.Username = u.Username

			proposals[k] = *pr
		}

		// Update the index
		startIdx = endIdx
	}

	return proposals, nil
}

func (p *Politeiawww) processTokenInventory(ctx context.Context, isAdmin bool) (*www.TokenInventoryReply, error) {
	log.Tracef("processTokenInventory")

	// Get record inventory
	ir, err := p.politeiad.Inventory(ctx, pdv2.RecordStateInvalid,
		pdv2.RecordStatusInvalid, 0)
	if err != nil {
		return nil, err
	}

	// Get vote inventory
	ti := ticketvote.Inventory{}
	vir, err := p.politeiad.TicketVoteInventory(ctx, ti)
	if err != nil {
		return nil, err
	}

	var (
		// Unvetted
		statusUnreviewed = pdv2.RecordStatuses[pdv2.RecordStatusUnreviewed]
		statusCensored   = pdv2.RecordStatuses[pdv2.RecordStatusCensored]
		statusArchived   = pdv2.RecordStatuses[pdv2.RecordStatusArchived]

		unreviewed = ir.Unvetted[statusUnreviewed]
		censored   = ir.Unvetted[statusCensored]

		// Human readable vote statuses
		statusUnauth   = tkplugin.VoteStatuses[tkplugin.VoteStatusUnauthorized]
		statusAuth     = tkplugin.VoteStatuses[tkplugin.VoteStatusAuthorized]
		statusStarted  = tkplugin.VoteStatuses[tkplugin.VoteStatusStarted]
		statusApproved = tkplugin.VoteStatuses[tkplugin.VoteStatusApproved]
		statusRejected = tkplugin.VoteStatuses[tkplugin.VoteStatusRejected]

		// Vetted
		unauth    = vir.Tokens[statusUnauth]
		auth      = vir.Tokens[statusAuth]
		pre       = append(unauth, auth...)
		active    = vir.Tokens[statusStarted]
		approved  = vir.Tokens[statusApproved]
		rejected  = vir.Tokens[statusRejected]
		abandoned = ir.Vetted[statusArchived]
	)

	// Only return unvetted tokens to admins
	if isAdmin {
		unreviewed = []string{}
		censored = []string{}
	}

	// Return empty arrays and not nils
	if unreviewed == nil {
		unreviewed = []string{}
	}
	if censored == nil {
		censored = []string{}
	}
	if pre == nil {
		pre = []string{}
	}
	if active == nil {
		active = []string{}
	}
	if approved == nil {
		approved = []string{}
	}
	if rejected == nil {
		rejected = []string{}
	}
	if abandoned == nil {
		abandoned = []string{}
	}

	return &www.TokenInventoryReply{
		Unreviewed: unreviewed,
		Censored:   censored,
		Pre:        pre,
		Active:     active,
		Approved:   approved,
		Rejected:   rejected,
		Abandoned:  abandoned,
	}, nil
}

func (p *Politeiawww) processAllVetted(ctx context.Context, gav www.GetAllVetted) (*www.GetAllVettedReply, error) {
	log.Tracef("processAllVetted: %v %v", gav.Before, gav.After)

	// NOTE: this route is not scalable and needs to be removed ASAP.
	// It only needs to be supported to give dcrdata a change to switch
	// to the records API.

	// The Before and After arguments are NO LONGER SUPPORTED. This
	// route will only return a single page of vetted tokens. The
	// records API InventoryOrdered command should be used instead.
	tokens, err := p.politeiad.InventoryOrdered(ctx, pdv2.RecordStateVetted, 1)
	if err != nil {
		return nil, err
	}

	// Get the proposals without any files
	reqs := make([]pdv2.RecordRequest, 0, pdv2.RecordsPageSize)
	for _, v := range tokens {
		reqs = append(reqs, pdv2.RecordRequest{
			Token: v,
			Filenames: []string{
				piplugin.FileNameProposalMetadata,
				tkplugin.FileNameVoteMetadata,
			},
		})
	}
	props, err := p.proposals(ctx, reqs)
	if err != nil {
		return nil, err
	}

	// Covert proposal map to an slice
	proposals := make([]www.ProposalRecord, 0, len(props))
	for _, v := range tokens {
		pr, ok := props[v]
		if !ok {
			continue
		}
		proposals = append(proposals, pr)
	}

	return &www.GetAllVettedReply{
		Proposals: proposals,
	}, nil
}

func (p *Politeiawww) processProposalDetails(ctx context.Context, pd www.ProposalsDetails, u *user.User) (*www.ProposalDetailsReply, error) {
	log.Tracef("processProposalDetails: %v", pd.Token)

	// Parse version
	var version uint64
	var err error
	if pd.Version != "" {
		version, err = strconv.ParseUint(pd.Version, 10, 64)
		if err != nil {
			return nil, www.UserError{
				ErrorCode: www.ErrorStatusProposalNotFound,
			}
		}
	}

	// Get proposal
	reqs := []pdv2.RecordRequest{
		{
			Token:   pd.Token,
			Version: uint32(version),
		},
	}
	prs, err := p.proposals(ctx, reqs)
	if err != nil {
		return nil, err
	}
	pr, ok := prs[pd.Token]
	if !ok {
		return nil, www.UserError{
			ErrorCode: www.ErrorStatusProposalNotFound,
		}
	}

	return &www.ProposalDetailsReply{
		Proposal: pr,
	}, nil
}

func (p *Politeiawww) processBatchProposals(ctx context.Context, bp www.BatchProposals, u *user.User) (*www.BatchProposalsReply, error) {
	log.Tracef("processBatchProposals: %v", bp.Tokens)

	if len(bp.Tokens) > www.ProposalListPageSize {
		return nil, www.UserError{
			ErrorCode: www.ErrorStatusMaxProposalsExceededPolicy,
		}
	}

	// Get the proposals batch
	reqs := make([]pdv2.RecordRequest, 0, len(bp.Tokens))
	for _, v := range bp.Tokens {
		reqs = append(reqs, pdv2.RecordRequest{
			Token: v,
			Filenames: []string{
				piplugin.FileNameProposalMetadata,
				tkplugin.FileNameVoteMetadata,
			},
		})
	}
	props, err := p.proposals(ctx, reqs)
	if err != nil {
		return nil, err
	}

	// Return the proposals in the same order they were requests in.
	proposals := make([]www.ProposalRecord, 0, len(props))
	for _, v := range bp.Tokens {
		pr, ok := props[v]
		if !ok {
			continue
		}
		proposals = append(proposals, pr)
	}

	return &www.BatchProposalsReply{
		Proposals: proposals,
	}, nil
}

func (p *Politeiawww) processBatchVoteSummary(ctx context.Context, bvs www.BatchVoteSummary) (*www.BatchVoteSummaryReply, error) {
	log.Tracef("processBatchVoteSummary: %v", bvs.Tokens)

	if len(bvs.Tokens) > www.ProposalListPageSize {
		return nil, www.UserError{
			ErrorCode: www.ErrorStatusMaxProposalsExceededPolicy,
		}
	}

	// Get vote summaries
	vs, err := p.politeiad.TicketVoteSummaries(ctx, bvs.Tokens)
	if err != nil {
		return nil, err
	}

	// Prepare reply
	var bestBlock uint32
	summaries := make(map[string]www.VoteSummary, len(vs))
	for token, v := range vs {
		bestBlock = v.BestBlock
		results := make([]www.VoteOptionResult, len(v.Results))
		for k, r := range v.Results {
			results[k] = www.VoteOptionResult{
				VotesReceived: r.Votes,
				Option: www.VoteOption{
					Id:          r.ID,
					Description: r.Description,
					Bits:        r.VoteBit,
				},
			}
		}
		summaries[token] = www.VoteSummary{
			Status:           convertVoteStatusToWWW(v.Status),
			Type:             convertVoteTypeToWWW(v.Type),
			Approved:         v.Status == tkplugin.VoteStatusApproved,
			EligibleTickets:  v.EligibleTickets,
			Duration:         v.Duration,
			EndHeight:        uint64(v.EndBlockHeight),
			QuorumPercentage: v.QuorumPercentage,
			PassPercentage:   v.PassPercentage,
			Results:          results,
		}
	}

	return &www.BatchVoteSummaryReply{
		Summaries: summaries,
		BestBlock: uint64(bestBlock),
	}, nil
}

func (p *Politeiawww) processVoteStatus(ctx context.Context, token string) (*www.VoteStatusReply, error) {
	log.Tracef("processVoteStatus")

	// Get vote summaries
	summaries, err := p.politeiad.TicketVoteSummaries(ctx, []string{token})
	if err != nil {
		return nil, err
	}
	s, ok := summaries[token]
	if !ok {
		return nil, www.UserError{
			ErrorCode: www.ErrorStatusProposalNotFound,
		}
	}
	vsr := convertVoteStatusReply(token, s)

	return &vsr, nil
}

func (p *Politeiawww) processAllVoteStatus(ctx context.Context) (*www.GetAllVoteStatusReply, error) {
	log.Tracef("processAllVoteStatus")

	// NOTE: This route is suppose to return the vote status of all
	// public proposals. This is horrendously unscalable. We are only
	// supporting this route until dcrdata has a chance to update and
	// use the ticketvote API. Until then, we only return a single page
	// of vote statuses.

	// Get a page of vetted records
	tokens, err := p.politeiad.InventoryOrdered(ctx, pdv2.RecordStateVetted, 1)
	if err != nil {
		return nil, err
	}

	// Get vote summaries
	vs, err := p.politeiad.TicketVoteSummaries(ctx, tokens)
	if err != nil {
		return nil, err
	}

	// Prepare reply
	statuses := make([]www.VoteStatusReply, 0, len(vs))
	for token, v := range vs {
		statuses = append(statuses, convertVoteStatusReply(token, v))
	}

	return &www.GetAllVoteStatusReply{
		VotesStatus: statuses,
	}, nil
}

func convertVoteDetails(vd tkplugin.VoteDetails) (www.StartVote, www.StartVoteReply) {
	options := make([]www.VoteOption, 0, len(vd.Params.Options))
	for _, v := range vd.Params.Options {
		options = append(options, www.VoteOption{
			Id:          v.ID,
			Description: v.Description,
			Bits:        v.Bit,
		})
	}
	sv := www.StartVote{
		Vote: www.Vote{
			Token:            vd.Params.Token,
			Mask:             vd.Params.Mask,
			Duration:         vd.Params.Duration,
			QuorumPercentage: vd.Params.QuorumPercentage,
			PassPercentage:   vd.Params.PassPercentage,
			Options:          options,
		},
		PublicKey: vd.PublicKey,
		Signature: vd.Signature,
	}
	svr := www.StartVoteReply{
		StartBlockHeight: strconv.FormatUint(uint64(vd.StartBlockHeight), 10),
		StartBlockHash:   vd.StartBlockHash,
		EndHeight:        strconv.FormatUint(uint64(vd.EndBlockHeight), 10),
		EligibleTickets:  vd.EligibleTickets,
	}

	return sv, svr
}

func (p *Politeiawww) processActiveVote(ctx context.Context) (*www.ActiveVoteReply, error) {
	log.Tracef("processActiveVotes")

	// Get a page of ongoing votes. This route is deprecated and should
	// be deleted before the time comes when more than a page of ongoing
	// votes is required.
	i := ticketvote.Inventory{}
	ir, err := p.politeiad.TicketVoteInventory(ctx, i)
	if err != nil {
		return nil, err
	}
	s := ticketvote.VoteStatuses[ticketvote.VoteStatusStarted]
	started := ir.Tokens[s]

	if len(started) == 0 {
		// No active votes
		return &www.ActiveVoteReply{
			Votes: []www.ProposalVoteTuple{},
		}, nil
	}

	// Get proposals
	reqs := make([]pdv2.RecordRequest, 0, pdv2.RecordsPageSize)
	for _, v := range started {
		reqs = append(reqs, pdv2.RecordRequest{
			Token: v,
			Filenames: []string{
				piplugin.FileNameProposalMetadata,
				tkplugin.FileNameVoteMetadata,
			},
		})
	}
	props, err := p.proposals(ctx, reqs)
	if err != nil {
		return nil, err
	}

	// Get vote details
	voteDetails := make(map[string]tkplugin.VoteDetails, len(started))
	for _, v := range started {
		dr, err := p.politeiad.TicketVoteDetails(ctx, v)
		if err != nil {
			return nil, err
		}
		if dr.Vote == nil {
			continue
		}
		voteDetails[v] = *dr.Vote
	}

	// Prepare reply
	votes := make([]www.ProposalVoteTuple, 0, len(started))
	for _, v := range started {
		var (
			proposal www.ProposalRecord
			sv       www.StartVote
			svr      www.StartVoteReply
			ok       bool
		)
		proposal, ok = props[v]
		if !ok {
			continue
		}
		vd, ok := voteDetails[v]
		if ok {
			sv, svr = convertVoteDetails(vd)
			votes = append(votes, www.ProposalVoteTuple{
				Proposal:       proposal,
				StartVote:      sv,
				StartVoteReply: svr,
			})
		}
	}

	return &www.ActiveVoteReply{
		Votes: votes,
	}, nil
}

func (p *Politeiawww) processCastVotes(ctx context.Context, ballot *www.Ballot) (*www.BallotReply, error) {
	log.Tracef("processCastVotes")

	// Verify there is work to do
	if len(ballot.Votes) == 0 {
		return &www.BallotReply{
			Receipts: []www.CastVoteReply{},
		}, nil
	}

	// Prepare plugin command
	votes := make([]tkplugin.CastVote, 0, len(ballot.Votes))
	var token string
	for _, v := range ballot.Votes {
		token = v.Token
		votes = append(votes, tkplugin.CastVote{
			Token:     v.Token,
			Ticket:    v.Ticket,
			VoteBit:   v.VoteBit,
			Signature: v.Signature,
		})
	}
	cb := tkplugin.CastBallot{
		Ballot: votes,
	}

	// Send plugin command
	cbr, err := p.politeiad.TicketVoteCastBallot(ctx, token, cb)
	if err != nil {
		return nil, err
	}

	// Prepare reply
	receipts := make([]www.CastVoteReply, 0, len(cbr.Receipts))
	for k, v := range cbr.Receipts {
		receipts = append(receipts, www.CastVoteReply{
			ClientSignature: ballot.Votes[k].Signature,
			Signature:       v.Receipt,
			Error:           v.ErrorContext,
			ErrorStatus:     convertVoteErrorCodeToWWW(v.ErrorCode),
		})
	}

	return &www.BallotReply{
		Receipts: receipts,
	}, nil
}

func (p *Politeiawww) processVoteResults(ctx context.Context, token string) (*www.VoteResultsReply, error) {
	log.Tracef("processVoteResults: %v", token)

	// Get vote details
	dr, err := p.politeiad.TicketVoteDetails(ctx, token)
	if err != nil {
		return nil, err
	}
	if dr.Vote == nil {
		return &www.VoteResultsReply{}, nil
	}
	sv, svr := convertVoteDetails(*dr.Vote)

	// Get cast votes
	rr, err := p.politeiad.TicketVoteResults(ctx, token)
	if err != nil {
		return nil, err
	}

	// Convert to www
	votes := make([]www.CastVote, 0, len(rr.Votes))
	for _, v := range rr.Votes {
		votes = append(votes, www.CastVote{
			Token:     v.Token,
			Ticket:    v.Ticket,
			VoteBit:   v.VoteBit,
			Signature: v.Signature,
		})
	}

	return &www.VoteResultsReply{
		StartVote:      sv,
		StartVoteReply: svr,
		CastVotes:      votes,
	}, nil
}

// userMetadataDecode decodes and returns the UserMetadata from the provided
// metadata streams. If a UserMetadata is not found, nil is returned.
func userMetadataDecode(ms []pdv2.MetadataStream) (*umplugin.UserMetadata, error) {
	var userMD *umplugin.UserMetadata
	for _, v := range ms {
		if v.PluginID != usermd.PluginID ||
			v.StreamID != umplugin.StreamIDUserMetadata {
			// This is not user metadata
			continue
		}
		var um umplugin.UserMetadata
		err := json.Unmarshal([]byte(v.Payload), &um)
		if err != nil {
			return nil, err
		}
		userMD = &um
		break
	}
	return userMD, nil
}

// userIDFromMetadataStreams searches for a UserMetadata and parses the user ID
// from it if found. An empty string is returned if no UserMetadata is found.
func userIDFromMetadataStreams(ms []pdv2.MetadataStream) string {
	um, err := userMetadataDecode(ms)
	if err != nil {
		return ""
	}
	if um == nil {
		return ""
	}
	return um.UserID
}

func convertStatusToWWW(status pdv2.RecordStatusT) www.PropStatusT {
	switch status {
	case pdv2.RecordStatusInvalid:
		return www.PropStatusInvalid
	case pdv2.RecordStatusPublic:
		return www.PropStatusPublic
	case pdv2.RecordStatusCensored:
		return www.PropStatusCensored
	case pdv2.RecordStatusArchived:
		return www.PropStatusAbandoned
	default:
		return www.PropStatusInvalid
	}
}

func convertRecordToProposal(r pdv2.Record) (*www.ProposalRecord, error) {
	// Decode metadata
	var (
		um       *umplugin.UserMetadata
		statuses = make([]umplugin.StatusChangeMetadata, 0, 16)
	)
	for _, v := range r.Metadata {
		if v.PluginID != umplugin.PluginID {
			continue
		}

		// This is a usermd plugin metadata stream
		switch v.StreamID {
		case umplugin.StreamIDUserMetadata:
			var m umplugin.UserMetadata
			err := json.Unmarshal([]byte(v.Payload), &m)
			if err != nil {
				return nil, err
			}
			um = &m
		case umplugin.StreamIDStatusChanges:
			d := json.NewDecoder(strings.NewReader(v.Payload))
			for {
				var sc umplugin.StatusChangeMetadata
				err := d.Decode(&sc)
				if errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return nil, err
				}
				statuses = append(statuses, sc)
			}
		}
	}

	// Convert files
	var (
		name, linkTo string
		linkBy       int64
		files        = make([]www.File, 0, len(r.Files))
	)
	for _, v := range r.Files {
		switch v.Name {
		case piplugin.FileNameProposalMetadata:
			b, err := base64.StdEncoding.DecodeString(v.Payload)
			if err != nil {
				return nil, err
			}
			var pm piplugin.ProposalMetadata
			err = json.Unmarshal(b, &pm)
			if err != nil {
				return nil, err
			}
			name = pm.Name

		case tkplugin.FileNameVoteMetadata:
			b, err := base64.StdEncoding.DecodeString(v.Payload)
			if err != nil {
				return nil, err
			}
			var vm tkplugin.VoteMetadata
			err = json.Unmarshal(b, &vm)
			if err != nil {
				return nil, err
			}
			linkTo = vm.LinkTo
			linkBy = vm.LinkBy

		default:
			files = append(files, www.File{
				Name:    v.Name,
				MIME:    v.MIME,
				Digest:  v.Digest,
				Payload: v.Payload,
			})
		}
	}

	// Setup user defined metadata
	pm := www.ProposalMetadata{
		Name:   name,
		LinkTo: linkTo,
		LinkBy: linkBy,
	}
	b, err := json.Marshal(pm)
	if err != nil {
		return nil, err
	}
	metadata := []www.Metadata{
		{
			Digest:  hex.EncodeToString(util.Digest(b)),
			Hint:    www.HintProposalMetadata,
			Payload: base64.StdEncoding.EncodeToString(b),
		},
	}

	var (
		publishedAt, censoredAt, abandonedAt int64
		changeMsg                            string
		changeMsgTimestamp                   int64
	)
	for _, v := range statuses {
		if v.Timestamp > changeMsgTimestamp {
			changeMsg = v.Reason
			changeMsgTimestamp = v.Timestamp
		}
		switch rcv1.RecordStatusT(v.Status) {
		case rcv1.RecordStatusPublic:
			publishedAt = v.Timestamp
		case rcv1.RecordStatusCensored:
			censoredAt = v.Timestamp
		case rcv1.RecordStatusArchived:
			abandonedAt = v.Timestamp
		}
	}

	return &www.ProposalRecord{
		Name:                pm.Name,
		State:               www.PropStateVetted,
		Status:              convertStatusToWWW(r.Status),
		Timestamp:           r.Timestamp,
		UserId:              um.UserID,
		Username:            "", // Intentionally omitted
		PublicKey:           um.PublicKey,
		Signature:           um.Signature,
		Version:             strconv.FormatUint(uint64(r.Version), 10),
		StatusChangeMessage: changeMsg,
		PublishedAt:         publishedAt,
		CensoredAt:          censoredAt,
		AbandonedAt:         abandonedAt,
		LinkTo:              pm.LinkTo,
		LinkBy:              pm.LinkBy,
		LinkedFrom:          []string{},
		Files:               files,
		Metadata:            metadata,
		CensorshipRecord: www.CensorshipRecord{
			Token:     r.CensorshipRecord.Token,
			Merkle:    r.CensorshipRecord.Merkle,
			Signature: r.CensorshipRecord.Signature,
		},
	}, nil
}

func convertVoteStatusToWWW(status tkplugin.VoteStatusT) www.PropVoteStatusT {
	switch status {
	case tkplugin.VoteStatusInvalid:
		return www.PropVoteStatusInvalid
	case tkplugin.VoteStatusUnauthorized:
		return www.PropVoteStatusNotAuthorized
	case tkplugin.VoteStatusAuthorized:
		return www.PropVoteStatusAuthorized
	case tkplugin.VoteStatusStarted:
		return www.PropVoteStatusStarted
	case tkplugin.VoteStatusFinished:
		return www.PropVoteStatusFinished
	case tkplugin.VoteStatusApproved:
		return www.PropVoteStatusFinished
	case tkplugin.VoteStatusRejected:
		return www.PropVoteStatusFinished
	default:
		return www.PropVoteStatusInvalid
	}
}

func convertVoteTypeToWWW(t tkplugin.VoteT) www.VoteT {
	switch t {
	case tkplugin.VoteTypeInvalid:
		return www.VoteTypeInvalid
	case tkplugin.VoteTypeStandard:
		return www.VoteTypeStandard
	case tkplugin.VoteTypeRunoff:
		return www.VoteTypeRunoff
	default:
		return www.VoteTypeInvalid
	}
}

func convertVoteErrorCodeToWWW(e *tkplugin.VoteErrorT) decredplugin.ErrorStatusT {
	if e == nil {
		return decredplugin.ErrorStatusInvalid
	}
	switch *e {
	case tkplugin.VoteErrorInvalid:
		return decredplugin.ErrorStatusInvalid
	case tkplugin.VoteErrorInternalError:
		return decredplugin.ErrorStatusInternalError
	case tkplugin.VoteErrorRecordNotFound:
		return decredplugin.ErrorStatusProposalNotFound
	case tkplugin.VoteErrorMultipleRecordVotes:
		// There is not decredplugin error code for this
	case tkplugin.VoteErrorVoteStatusInvalid:
		return decredplugin.ErrorStatusVoteHasEnded
	case tkplugin.VoteErrorVoteBitInvalid:
		return decredplugin.ErrorStatusInvalidVoteBit
	case tkplugin.VoteErrorSignatureInvalid:
		// There is not decredplugin error code for this
	case tkplugin.VoteErrorTicketNotEligible:
		return decredplugin.ErrorStatusIneligibleTicket
	case tkplugin.VoteErrorTicketAlreadyVoted:
		return decredplugin.ErrorStatusDuplicateVote
	default:
	}
	return decredplugin.ErrorStatusInternalError
}

func convertVoteStatusReply(token string, s tkplugin.SummaryReply) www.VoteStatusReply {
	results := make([]www.VoteOptionResult, 0, len(s.Results))
	var totalVotes uint64
	for _, v := range s.Results {
		totalVotes += v.Votes
		results = append(results, www.VoteOptionResult{
			VotesReceived: v.Votes,
			Option: www.VoteOption{
				Id:          v.ID,
				Description: v.Description,
				Bits:        v.VoteBit,
			},
		})
	}
	return www.VoteStatusReply{
		Token:              token,
		Status:             convertVoteStatusToWWW(s.Status),
		TotalVotes:         totalVotes,
		OptionsResult:      results,
		EndHeight:          strconv.FormatUint(uint64(s.EndBlockHeight), 10),
		BestBlock:          strconv.FormatUint(uint64(s.BestBlock), 10),
		NumOfEligibleVotes: int(s.EligibleTickets),
		QuorumPercentage:   s.QuorumPercentage,
		PassPercentage:     s.PassPercentage,
	}
}
