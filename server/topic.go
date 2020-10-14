/******************************************************************************
 *
 *  Description :
 *    An isolated communication channel (chat room, 1:1 conversation) for
 *    usually multiple users. There is no communication across topics.
 *
 *****************************************************************************/

package main

import (
	"errors"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tinode/chat/server/auth"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/types"
)

// Topic is an isolated communication channel
type Topic struct {
	// Еxpanded/unique name of the topic.
	name string
	// For single-user topics session-specific topic name, such as 'me',
	// otherwise the same as 'name'.
	xoriginal string

	// Topic category
	cat types.TopicCat

	// Channel functionality is enabled for the group topic.
	isChan bool

	// If isProxy == true, the actual topic is hosted by another cluster member.
	// The topic should:
	// 1. forward all messages to master
	// 2. route replies from the master to sessions.
	// 3. disconnect sessions at master's request.
	// 4. shut down the topic at master's request.
	// 5. aggregate access permissions on behalf of attached sessions.
	isProxy bool
	// Name of the master node for this topic if isProxy is true.
	masterNode string
	// Topic runs a goroutine (clusterWriteLoop) that reads events from all proxy
	// multiplexing sessions.
	// List of proxied sessions.
	proxiedSessions []*Session
	// Proxied sessions' channels for the use in the topic's clusterWriteLoop:
	// i-th session's channels (proxiedSessions[i]) are found at:
	// proxiedChannels[i * 3 + 1] - send
	// proxiedChannels[i * 3 + 2] - stop
	// proxiedChannels[i * 3 + 3] - detach
	//
	// proxiedChannels[0] is a special-purpose channel necessary for interrupting
	// clusterWriteLoop when sessions are added or removed.
	proxiedChannels []reflect.SelectCase

	// Time when the topic was first created.
	created time.Time
	// Time when the topic was last updated.
	updated time.Time
	// Time of the last outgoing message.
	touched time.Time

	// Server-side ID of the last data message
	lastID int
	// ID of the deletion operation. Not an ID of the message.
	delID int

	// Last published userAgent ('me' topic only)
	userAgent string

	// User ID of the topic owner/creator. Could be zero.
	owner types.Uid

	// Default access mode
	accessAuth types.AccessMode
	accessAnon types.AccessMode

	// Topic discovery tags
	tags []string

	// Topic's public data
	public interface{}

	// Topic's per-subscriber data
	perUser map[types.Uid]perUserData
	// Union of permissions across all users (used by proxy sessions with uid = 0).
	// These are used by master topics only (in the proxy-master topic context)
	// as a coarse-grained attempt to perform acs checks since proxy sessions "impersonate"
	// multiple normal sessions (uids) which may have different uids.
	modeWantUnion  types.AccessMode
	modeGivenUnion types.AccessMode

	// User's contact list (not nil for 'me' topic only).
	// The map keys are UserIds for P2P topics and grpXXX for group topics.
	perSubs map[string]perSubsData

	// Sessions attached to this topic. The UID kept here may not match Session.uid if session is
	// subscribed on behalf of another user.
	sessions map[*Session]perSessionData

	// Requests to broadcast messages from sessions or other topics. Buffered = 256
	broadcast chan *ServerComMessage
	// Channel for receiving {get}/{set} requests, buffered = 32
	meta chan *metaReq
	// Subscribe requests from sessions, buffered = 32
	reg chan *sessionJoin
	// Unsubscribe requests from sessions, buffered = 32
	unreg chan *sessionLeave
	// Session updates: background sessions coming online, User Agent changes. Buffered = 32
	supd chan *sessionUpdate
	// Channel to terminate topic  -- either the topic is deleted or system is being shut down. Buffered = 1.
	exit chan *shutDown
	// Channel to receive topic master responses (used only by proxy topics).
	proxy chan *ClusterResp
	// Channel to receive topic proxy service requests, e.g. sending deferred notifications.
	master chan *ClusterSessUpdate

	// Flag which tells topic lifecycle status: new, ready, paused, marked for deletion.
	status int32
}

// perUserData holds topic's cache of per-subscriber data
type perUserData struct {
	// Timestamps when the subscription was created and updated
	created time.Time
	updated time.Time

	// Count of subscription online and announced (presence not deferred).
	online int

	// Last t.lastId reported by user through {pres} as received or read
	recvID int
	readID int
	// ID of the latest Delete operation
	delID int

	private interface{}

	modeWant  types.AccessMode
	modeGiven types.AccessMode

	// P2P only:
	public    interface{}
	topicName string
	deleted   bool
}

// perSubsData holds user's (on 'me' topic) cache of subscription data
type perSubsData struct {
	// The other user's/topic's online status as seen by this user.
	online bool
	// True if we care about the updates from the other user/topic: (want&given).IsPresencer().
	// Does not affect sending notifications from this user to other users.
	enabled bool
}

// Data related to a subscription of a session to a topic.
type perSessionData struct {
	// ID of the subscribed user (asUid); not necessarily the session owner.
	// Could be zero for multiplexed sessions in cluster.
	uid types.Uid
	// This is a channel subscription
	isChanSub bool
	// IDs of subscribed users in a multiplexing session.
	muids []types.Uid
}

// Reasons why topic is being shut down.
const (
	// StopNone no reason given/default.
	StopNone = iota
	// StopShutdown terminated due to system shutdown.
	StopShutdown
	// StopDeleted terminated due to being deleted.
	StopDeleted
	// StopRehashing terminated due to cluster rehashing (moved to a different node).
	StopRehashing
)

// Topic shutdown
type shutDown struct {
	// Channel to report back completion of topic shutdown. Could be nil
	done chan<- bool
	// Topic is being deleted as opposite to total system shutdown
	reason int
}

// Session update: user agent change or background session becoming normal.
// If sess is nil then user agent change, otherwise bg to fg update.
type sessionUpdate struct {
	sess      *Session
	userAgent string
}

var nilPresParams = &presParams{}
var nilPresFilters = &presFilters{}

func (t *Topic) run(hub *Hub) {
	if !t.isProxy {
		t.runLocal(hub)
	} else {
		t.runProxy(hub)
	}
}

// getPerUserAcs returns `want` and `given` permissions for the given user id.
func (t *Topic) getPerUserAcs(uid types.Uid) (types.AccessMode, types.AccessMode) {
	if uid.IsZero() {
		// For zero uids (typically for proxy sessions), return the union of all permissions.
		return t.modeWantUnion, t.modeGivenUnion
	}
	pud := t.perUser[uid]
	return pud.modeWant, pud.modeGiven
}

// passesPresenceFilters applies presence filters to `msg`
// depending on per-user want and given acls for the provided `uid`.
func (t *Topic) passesPresenceFilters(pres *MsgServerPres, uid types.Uid) bool {
	modeWant, modeGiven := t.getPerUserAcs(uid)
	// "gone" and "acs" notifications are sent even if the topic is muted.
	return ((modeGiven & modeWant).IsPresencer() || pres.What == "gone" || pres.What == "acs") &&
		(pres.FilterIn == 0 || int(modeGiven&modeWant)&pres.FilterIn != 0) &&
		(pres.FilterOut == 0 || int(modeGiven&modeWant)&pres.FilterOut == 0)
}

// userIsReader returns true if the user (specified by `uid`) may read the given topic.
func (t *Topic) userIsReader(uid types.Uid) bool {
	modeWant, modeGiven := t.getPerUserAcs(uid)
	return (modeGiven & modeWant).IsReader()
}

// maybeFixTopicName sets the topic field in `msg` depending on the uid.
func (t *Topic) maybeFixTopicName(msg *ServerComMessage, uid types.Uid) {
	// For zero uids we don't know the proper topic name.
	if uid.IsZero() {
		return
	}

	if t.cat == types.TopicCatP2P || (t.cat == types.TopicCatGrp && t.isChan) {
		// For p2p topics topic name is dependent on receiver.
		// Channel topics may be presented as grpXXX or chnXXX.
		switch {
		case msg.Data != nil:
			msg.Data.Topic = t.original(uid)
		case msg.Pres != nil:
			msg.Pres.Topic = t.original(uid)
		case msg.Info != nil:
			msg.Info.Topic = t.original(uid)
		}
	}
}

// computePerUserAcsUnion computes want and given permissions unions over all topic's subscribers.
func (t *Topic) computePerUserAcsUnion() {
	wantUnion := types.ModeNone
	givenUnion := types.ModeNone
	for _, pud := range t.perUser {
		wantUnion = wantUnion | pud.modeWant
		givenUnion = givenUnion | pud.modeGiven
	}
	t.modeWantUnion = wantUnion
	t.modeGivenUnion = givenUnion
}

// fixUpUserCounts decrements online counts for the provided user ids.
func (t *Topic) fixUpUserCounts(userCounts map[types.Uid]int) {
	for uid, decrementBy := range userCounts {
		if pud, ok := t.perUser[uid]; ok {
			pud.online -= decrementBy
			t.perUser[uid] = pud
			if pud.online < 0 {
				log.Printf("topic[%s]: invalid online count for user %s", t.name, uid)
			}
		}
	}
}

func (t *Topic) runLocal(hub *Hub) {
	// Kills topic after a period of inactivity.
	keepAlive := idleMasterTopicTimeout
	killTimer := time.NewTimer(time.Hour)
	killTimer.Stop()

	// Notifies about user agent change. 'me' only
	uaTimer := time.NewTimer(time.Minute)
	var currentUA string
	uaTimer.Stop()

	// Ticker for deferred presence notifications.
	defrNotifTimer := time.NewTimer(time.Millisecond * 500)

	for {
		select {
		case join := <-t.reg:
			// Request to add a connection to this topic
			if t.isInactive() {
				join.sess.queueOut(ErrLockedReply(join.pkt, types.TimeNow()))
			} else {
				// The topic is alive, so stop the kill timer, if it's ticking. We don't want the topic to die
				// while processing the call
				killTimer.Stop()
				if err := t.handleSubscription(hub, join); err == nil {
					if join.pkt.Sub.Created {
						// Call plugins with the new topic
						pluginTopic(t, plgActCreate)
					}
				} else {
					if len(t.sessions) == 0 && t.cat != types.TopicCatSys {
						// Failed to subscribe, the topic is still inactive
						killTimer.Reset(keepAlive)
					}
					log.Printf("topic[%s] subscription failed %v, sid=%s", t.name, err, join.sess.sid)
				}
			}
			if join.sess.inflightReqs != nil {
				join.sess.inflightReqs.Done()
			}
		case leave := <-t.unreg:
			t.handleLeaveRequest(hub, leave)
			if leave.pkt != nil && leave.sess.inflightReqs != nil {
				// If it's a client initiated request.
				leave.sess.inflightReqs.Done()
			}

			// If there are no more subscriptions to this topic, start a kill timer
			if len(t.sessions) == 0 && t.cat != types.TopicCatSys {
				killTimer.Reset(keepAlive)
			}

		case msg := <-t.broadcast:
			// Content message intended for broadcasting to recipients
			t.handleBroadcast(msg)

		case meta := <-t.meta:
			// Request to get/set topic metadata
			asUid := types.ParseUserId(meta.pkt.AsUser)
			authLevel := auth.Level(meta.pkt.AuthLvl)
			switch {
			case meta.pkt.Get != nil:
				// Get request
				if meta.pkt.MetaWhat&constMsgMetaDesc != 0 {
					if err := t.replyGetDesc(meta.sess, asUid, meta.pkt.Get.Desc, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Desc failed: %s", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaSub != 0 {
					if err := t.replyGetSub(meta.sess, asUid, authLevel, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Sub failed: %s", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaData != 0 {
					if err := t.replyGetData(meta.sess, asUid, meta.pkt.Get.Data, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Data failed: %s", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaDel != 0 {
					if err := t.replyGetDel(meta.sess, asUid, meta.pkt.Get.Del, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Del failed: %s", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaTags != 0 {
					if err := t.replyGetTags(meta.sess, asUid, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Tags failed: %s", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaCred != 0 {
					log.Printf("topic[%s] handle getCred", t.name)
					if err := t.replyGetCreds(meta.sess, asUid, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Get.Creds failed: %s", t.name, err)
					}
				}

			case meta.pkt.Set != nil:
				// Set request
				if meta.pkt.MetaWhat&constMsgMetaDesc != 0 {
					if err := t.replySetDesc(meta.sess, asUid, meta.pkt); err == nil {
						// Notify plugins of the update
						pluginTopic(t, plgActUpd)
					} else {
						log.Printf("topic[%s] meta.Set.Desc failed: %v", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaSub != 0 {
					if err := t.replySetSub(hub, meta.sess, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Set.Sub failed: %v", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaTags != 0 {
					if err := t.replySetTags(meta.sess, asUid, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Set.Tags failed: %v", t.name, err)
					}
				}
				if meta.pkt.MetaWhat&constMsgMetaCred != 0 {
					if err := t.replySetCred(meta.sess, asUid, authLevel, meta.pkt); err != nil {
						log.Printf("topic[%s] meta.Set.Cred failed: %v", t.name, err)
					}
				}

			case meta.pkt.Del != nil:
				// Del request
				var err error
				switch meta.pkt.MetaWhat {
				case constMsgDelMsg:
					err = t.replyDelMsg(meta.sess, asUid, meta.pkt)
				case constMsgDelSub:
					err = t.replyDelSub(hub, meta.sess, asUid, meta.pkt)
				case constMsgDelTopic:
					err = t.replyDelTopic(hub, meta.sess, asUid, meta.pkt)
				case constMsgDelCred:
					err = t.replyDelCred(hub, meta.sess, asUid, authLevel, meta.pkt)
				}

				if err != nil {
					log.Printf("topic[%s] meta.Del failed: %v", t.name, err)
				}
			}
		case upd := <-t.supd:
			if upd.sess != nil {
				// 'me' & 'grp' only. Background session timed out and came online.
				t.sessToForeground(upd.sess)
			} else if currentUA != upd.userAgent {
				if t.cat != types.TopicCatMe {
					log.Panicln("invalid topic category in UA update", t.name)
				}
				// 'me' only. Process an update to user agent from one of the sessions.
				currentUA = upd.userAgent
				uaTimer.Reset(uaTimerDelay)
			}

		case <-uaTimer.C:
			// Publish user agent changes after a delay
			if currentUA == "" || currentUA == t.userAgent {
				continue
			}
			t.userAgent = currentUA
			t.presUsersOfInterest("ua", t.userAgent)

		case <-killTimer.C:
			// Topic timeout
			hub.unreg <- &topicUnreg{rcptTo: t.name}
			defrNotifTimer.Stop()
			if t.cat == types.TopicCatMe {
				uaTimer.Stop()
				t.presUsersOfInterest("off", currentUA)
			} else if t.cat == types.TopicCatGrp {
				t.presSubsOffline("off", nilPresParams, nilPresFilters, nilPresFilters, "", false)
			}

		case sd := <-t.exit:
			// Handle four cases:
			// 1. Topic is shutting down by timer due to inactivity (reason == StopNone)
			// 2. Topic is being deleted (reason == StopDeleted)
			// 3. System shutdown (reason == StopShutdown, done != nil).
			// 4. Cluster rehashing (reason == StopRehashing)

			if sd.reason == StopDeleted {
				if t.cat == types.TopicCatGrp {
					t.presSubsOffline("gone", nilPresParams, nilPresFilters, nilPresFilters, "", false)
				}
				// P2P users get "off+remove" earlier in the process

				// Inform plugins that the topic is deleted
				pluginTopic(t, plgActDel)

			} else if sd.reason == StopRehashing {
				// Must send individual messages to sessions because normal sending through the topic's
				// broadcast channel won't work - it will be shut down too soon.
				t.presSubsOnlineDirect("term", nilPresParams, nilPresFilters, "")
			}
			// In case of a system shutdown don't bother with notifications. They won't be delivered anyway.

			// Tell sessions to remove the topic
			for s := range t.sessions {
				s.detachSession(t.name)
			}

			usersRegisterTopic(t, false)

			// Report completion back to sender, if 'done' is not nil.
			if sd.done != nil {
				sd.done <- true
			}
			return
		}
	}
}

// Session subscribed to a topic, created == true if topic was just created and {pres} needs to be announced
func (t *Topic) handleSubscription(h *Hub, join *sessionJoin) error {
	asUid := types.ParseUserId(join.pkt.AsUser)
	authLevel := auth.Level(join.pkt.AuthLvl)
	asChan := isChannel(join.pkt.Original)

	msgsub := join.pkt.Sub
	getWhat := 0
	if msgsub.Get != nil {
		getWhat = parseMsgClientMeta(msgsub.Get.What)
	}

	if err := t.subscriptionReply(h, asChan, join); err != nil {
		return err
	}

	if getWhat&constMsgMetaDesc != 0 {
		// Send get.desc as a {meta} packet.
		if err := t.replyGetDesc(join.sess, asUid, msgsub.Get.Desc, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Desc failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	if getWhat&constMsgMetaSub != 0 {
		// Send get.sub response as a separate {meta} packet
		if err := t.replyGetSub(join.sess, asUid, authLevel, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Sub failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	if getWhat&constMsgMetaTags != 0 {
		// Send get.tags response as a separate {meta} packet
		if err := t.replyGetTags(join.sess, asUid, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Tags failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	if getWhat&constMsgMetaCred != 0 {
		// Send get.tags response as a separate {meta} packet
		if err := t.replyGetCreds(join.sess, asUid, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Cred failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	if getWhat&constMsgMetaData != 0 {
		// Send get.data response as {data} packets
		if err := t.replyGetData(join.sess, asUid, msgsub.Get.Data, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Data failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	if getWhat&constMsgMetaDel != 0 {
		// Send get.del response as a separate {meta} packet
		if err := t.replyGetDel(join.sess, asUid, msgsub.Get.Del, join.pkt); err != nil {
			log.Printf("topic[%s] handleSubscription Get.Del failed: %v sid=%s", t.name, err, join.sess.sid)
		}
	}

	return nil
}

// handleLeaveRequest processes a session leave request.
func (t *Topic) handleLeaveRequest(hub *Hub, leave *sessionLeave) {
	// Remove connection from topic; session may continue to function
	now := types.TimeNow()

	// userId.IsZero() == true when the entire session is being dropped.
	var asUid types.Uid
	var asChan bool
	if leave.pkt != nil {
		asUid = types.ParseUserId(leave.pkt.AsUser)
		var err error
		asChan, err = t.verifyChannelAccess(leave.pkt.Original)
		if err != nil {
			// Group topic cannot be addressed as channel unless channel functionality is enabled.
			leave.sess.queueOut(ErrNotFoundReply(leave.pkt, now))
		}
	}

	if t.isInactive() {
		if !asUid.IsZero() && leave.pkt != nil {
			leave.sess.queueOut(ErrLockedReply(leave.pkt, now))
		}
		return
	} else if asChan && !t.isChan {
		if leave.pkt != nil {
			// Group topic cannot be addressed as channel unless channel functionality is enabled.
			leave.sess.queueOut(ErrNotFoundReply(leave.pkt, now))
		}
		return
	} else if leave.pkt != nil && leave.pkt.Leave.Unsub {
		// User wants to leave and unsubscribe.
		// asUid must not be Zero.
		if err := t.replyLeaveUnsub(hub, leave.sess, leave.pkt, asUid); err != nil {
			log.Println("failed to unsub", err, leave.sess.sid)
			return
		}
	} else if pssd, _ := t.remSession(leave.sess, asUid); pssd != nil {
		if pssd.isChanSub && asChan {
			if leave.pkt != nil {
				leave.sess.queueOut(NoErr(leave.pkt.Id, leave.pkt.Original, now))
			}
			return
		}

		if pssd.isChanSub != asChan {
			// Cannot address non-channel subscription as channel and vice versa.
			if leave.pkt != nil {
				// Group topic cannot be addressed as channel unless channel functionality is enabled.
				leave.sess.queueOut(ErrNotFoundReply(leave.pkt, now))
			}
			return
		}

		var uid types.Uid
		if leave.sess.isProxy() {
			// Multiplexing session, multiple UIDs.
			uid = asUid
		} else {
			// Simple session, single UID.
			uid = pssd.uid
		}

		var pud perUserData
		// uid may be zero when a proxy session is trying to terminate (it called unsubAll).
		if !uid.IsZero() {
			// UID not zero: one user removed.
			pud = t.perUser[uid]
			if !leave.sess.background {
				pud.online--
			}
		} else if len(pssd.muids) > 0 {
			// UID is zero: multiplexing session is dropped altogether.
			// Using new 'uid' and 'pud' variables.
			for _, uid := range pssd.muids {
				pud := t.perUser[uid]
				pud.online--
				t.perUser[uid] = pud
			}
		} else if !leave.sess.isCluster() {
			log.Panic("cannot determine uid: leave req=", leave)
		}

		switch t.cat {
		case types.TopicCatMe:
			mrs := t.mostRecentSession()
			if mrs == nil {
				// Last session
				mrs = leave.sess
			} else {
				// Change UA to the most recent live session and announce it. Don't block.
				select {
				case t.supd <- &sessionUpdate{userAgent: mrs.userAgent}:
				default:
				}
			}

			meUid := uid
			if meUid.IsZero() && len(pssd.muids) > 0 {
				// The entire multiplexing session is being dropped. Need to find owner's UID.
				// len(pssd.muids) could be zero if the session was a background session.
				meUid = pssd.muids[0]
			}
			if !meUid.IsZero() {
				// Update user's last online timestamp & user agent. Only one user can be subscribed to 'me' topic.
				if err := store.Users.UpdateLastSeen(meUid, mrs.userAgent, now); err != nil {
					log.Println(err)
				}
			}
		case types.TopicCatFnd:
			// FIXME: this does not work correctly in case of a multiplexing query.
			// Remove ephemeral query.
			t.fndRemovePublic(leave.sess)
		case types.TopicCatGrp:
			// Topic is going offline: notify online subscribers on 'me'.
			readFilter := &presFilters{filterIn: types.ModeRead}
			if !uid.IsZero() {
				if pud.online == 0 {
					t.presSubsOnline("off", uid.UserId(), nilPresParams, readFilter, "")
				}
			} else if len(pssd.muids) > 0 {
				for _, uid := range pssd.muids {
					if t.perUser[uid].online == 0 {
						t.presSubsOnline("off", uid.UserId(), nilPresParams, readFilter, "")
					}
				}
			}
		}

		if !uid.IsZero() {
			t.perUser[uid] = pud

			// Respond if contains an id.
			if leave.pkt != nil {
				leave.sess.queueOut(NoErrReply(leave.pkt, now))
			}
		}
	}
}

// sessToForeground updates perUser online status accounting and fires due
// deferred notifications for the provided session.
func (t *Topic) sessToForeground(sess *Session) {
	s := sess
	if s.multi != nil {
		s = s.multi
	}

	if pssd, ok := t.sessions[s]; ok && !pssd.isChanSub {
		uid := pssd.uid
		if s.isMultiplex() {
			// If 's' is a multiplexing session, then sess is a proxy and it contains correct UID.
			// Add UID to the list of online users.
			uid := sess.uid
			pssd.muids = append(pssd.muids, uid)
		}
		// Mark user as online
		pud := t.perUser[uid]
		pud.online++
		t.perUser[uid] = pud

		t.sendSubNotifications(uid, sess.sid, sess.userAgent)
	}
}

// Subscribe or unsubscribe user to/from FCM topic (channel).
func (t *Topic) channelSubUnsub(uid types.Uid, sub bool) {
	push.ChannelSub(&push.ChannelReq{
		Uid:     uid,
		Channel: types.GrpToChn(t.name),
		Unsub:   !sub})
}

// Send immediate presence notification in response to a subscription.
// Send push notification to the P2P counterpart.
// In case of a new channel subscription subscribe user to an FCM topic.
// These notifications are always sent immediately even if background is requested.
func (t *Topic) sendImmediateSubNotifications(asUid types.Uid, acs *MsgAccessMode, sreg *sessionJoin) {
	modeWant, _ := types.ParseAcs([]byte(acs.Want))
	modeGiven, _ := types.ParseAcs([]byte(acs.Given))
	mode := modeWant & modeGiven

	if t.cat == types.TopicCatP2P {
		uid2 := t.p2pOtherUser(asUid)
		pud2 := t.perUser[uid2]
		mode2 := pud2.modeGiven & pud2.modeWant
		if pud2.deleted {
			mode2 = types.ModeInvalid
		}

		// Inform the other user that the topic was just created.
		if sreg.pkt.Sub.Created {
			t.presSingleUserOffline(uid2, mode2, "acs", &presParams{
				dWant:  pud2.modeWant.String(),
				dGiven: pud2.modeGiven.String(),
				actor:  asUid.UserId()}, "", false)
		}

		if sreg.pkt.Sub.Newsub {
			// Notify current user's 'me' topic to accept notifications from user2
			t.presSingleUserOffline(asUid, mode, "?none+en", nilPresParams, "", false)

			// Initiate exchange of 'online' status with the other user.
			// We don't know if the current user is online in the 'me' topic,
			// so sending an '?unkn' status to user2. His 'me' topic
			// will reply with user2's status and request an actual status from user1.
			status := "?unkn"
			if mode2.IsPresencer() {
				// If user2 should receive notifications, enable it.
				status += "+en"
			}
			t.presSingleUserOffline(uid2, mode2, status, nilPresParams, "", false)

			// Also send a push notification to the other user.
			if pushRcpt := t.pushForSub(asUid, uid2, pud2.modeWant, pud2.modeGiven, types.TimeNow()); pushRcpt != nil {
				usersPush(pushRcpt)
			}
		}
	}

	// newsub could be true only for p2p and group topics, no need to check topic category explicitly.
	if sreg.pkt.Sub.Newsub {
		// Notify creator's other sessions that the subscription (or the entire topic) was created.
		t.presSingleUserOffline(asUid, mode, "acs",
			&presParams{
				dWant:  acs.Want,
				dGiven: acs.Given,
				actor:  asUid.UserId()},
			sreg.sess.sid, false)

		if t.isChan && isChannel(sreg.pkt.Original) {
			t.channelSubUnsub(asUid, true)
		}
	}
}

// Send immediate or deferred presence notification in response to a subscription.
// Not used by channels.
func (t *Topic) sendSubNotifications(asUid types.Uid, sid, userAgent string) {
	switch t.cat {
	case types.TopicCatMe:
		// Notify user's contact that the given user is online now.
		if !t.isLoaded() {
			t.markLoaded()
			if err := t.loadContacts(asUid); err != nil {
				log.Println("topic: failed to load contacts", t.name, err.Error())
			}
			// User online: notify users of interest without forcing response (no +en here).
			t.presUsersOfInterest("on", userAgent)
		}

	case types.TopicCatGrp:
		pud := t.perUser[asUid]

		// Enable notifications for a new group topic, if appropriate.
		if !t.isLoaded() {
			t.markLoaded()
			status := "on"
			if (pud.modeGiven & pud.modeWant).IsPresencer() {
				status += "+en"
			}

			// Notify topic subscribers that the topic is online now.
			t.presSubsOffline(status, nilPresParams, nilPresFilters, nilPresFilters, "", false)
		} else if pud.online == 1 {
			// If this is the first session of the user in the topic.
			// Notify other online group members that the user is online now.
			t.presSubsOnline("on", asUid.UserId(), nilPresParams,
				&presFilters{filterIn: types.ModeRead}, sid)
		}
	}
}

// handleBroadcast fans out broadcastable messages to recipients in topic and proxy_topic.
func (t *Topic) handleBroadcast(msg *ServerComMessage) {
	asUid := types.ParseUserId(msg.AsUser)
	if t.isInactive() {
		// Ignore broadcast - topic is paused or being deleted.
		if msg.Data != nil {
			msg.sess.queueOut(ErrLocked(msg.Id, t.original(asUid), msg.Timestamp))
		}
		return
	}

	var pushRcpt *push.Receipt
	if msg.Data != nil {
		if t.isReadOnly() {
			msg.sess.queueOut(ErrPermissionDenied(msg.Id, t.original(asUid), msg.Timestamp))
			return
		}

		asUser := types.ParseUserId(msg.Data.From)
		userData, userFound := t.perUser[asUser]
		// Anyone is allowed to post to 'sys' topic.
		if t.cat != types.TopicCatSys {
			// If it's not 'sys' check write permission.
			if !(userData.modeWant & userData.modeGiven).IsWriter() {
				msg.sess.queueOut(ErrPermissionDenied(msg.Id, t.original(asUid), msg.Timestamp))
				return
			}
		}

		if t.isProxy {
			t.lastID = msg.Data.SeqId
		} else {
			// Save to DB at master topic.
			if err := store.Messages.Save(&types.Message{
				ObjHeader: types.ObjHeader{CreatedAt: msg.Data.Timestamp},
				SeqId:     t.lastID + 1,
				Topic:     t.name,
				From:      asUser.String(),
				Head:      msg.Data.Head,
				Content:   msg.Data.Content}, (userData.modeGiven & userData.modeWant).IsReader()); err != nil {

				log.Printf("topic[%s]: failed to save message: %v", t.name, err)
				msg.sess.queueOut(ErrUnknown(msg.Id, t.original(asUid), msg.Timestamp))

				return
			}

			t.lastID++
			t.touched = msg.Data.Timestamp
			msg.Data.SeqId = t.lastID
		}

		if userFound {
			userData.readID = t.lastID
			userData.readID = t.lastID
			t.perUser[asUser] = userData
		}

		if msg.Id != "" && msg.sess != nil {
			reply := NoErrAccepted(msg.Id, t.original(asUid), msg.Timestamp)
			reply.Ctrl.Params = map[string]int{"seq": t.lastID}
			msg.sess.queueOut(reply)
		}

		if !t.isProxy {
			pushRcpt = t.pushForData(asUser, msg.Data)

			// Message sent: notify offline 'R' subscrbers on 'me'.
			t.presSubsOffline("msg", &presParams{seqID: t.lastID, actor: msg.Data.From},
				&presFilters{filterIn: types.ModeRead}, nilPresFilters, "", true)

			// Tell the plugins that a message was accepted for delivery
			pluginMessage(msg.Data, plgActCreate)
		}

	} else if msg.Pres != nil {
		what := t.presProcReq(msg.Pres.Src, msg.Pres.What, msg.Pres.WantReply)
		if t.xoriginal != msg.Pres.Topic || what == "" {
			// This is just a request for status, don't forward it to sessions
			return
		}

		// "what" may have changed, i.e. unset or "+command" removed ("on+en" -> "on")
		msg.Pres.What = what
	} else if msg.Info != nil {
		if msg.Info.SeqId > t.lastID {
			// Drop bogus read notification
			return
		}

		asUser := types.ParseUserId(msg.Info.From)
		pud := t.perUser[asUser]
		mode := pud.modeGiven & pud.modeWant
		if pud.deleted {
			mode = types.ModeInvalid
		}

		// Filter out "kp" from users with no 'W' permission (or people without a subscription)
		if msg.Info.What == "kp" && (!mode.IsWriter() || t.isReadOnly()) {
			return
		}

		if msg.Info.What == "read" || msg.Info.What == "recv" {
			// Filter out "read/recv" from users with no 'R' permission (or people without a subscription)
			if !mode.IsReader() {
				return
			}

			var read, recv, unread int
			if msg.Info.What == "read" {
				if msg.Info.SeqId > pud.readID {
					// The number of unread messages has decreased, negative value
					unread = pud.readID - msg.Info.SeqId
					pud.readID = msg.Info.SeqId
					read = pud.readID
				} else {
					// No need to report stale or bogus read status
					return
				}
			} else if msg.Info.What == "recv" {
				if msg.Info.SeqId > pud.recvID {
					pud.recvID = msg.Info.SeqId
					recv = pud.recvID
				} else {
					return
				}
			}

			if pud.readID > pud.recvID {
				pud.recvID = pud.readID
				recv = pud.recvID
			}

			if !t.isProxy {
				if err := store.Subs.Update(t.name, asUser,
					map[string]interface{}{
						"RecvSeqId": pud.recvID,
						"ReadSeqId": pud.readID},
					false); err != nil {

					log.Printf("topic[%s]: failed to update SeqRead/Recv counter: %v", t.name, err)
					return
				}

				// Read/recv updated: notify user's other sessions of the change
				t.presPubMessageCount(asUser, mode, recv, read, msg.SkipSid)

				// Update cached count of unread messages
				usersUpdateUnread(asUser, unread, true)
			}
			t.perUser[asUser] = pud
		}
	} else {
		// TODO(gene): remove this
		log.Panic("topic: wrong message type for broadcasting", t.name)
	}

	// Broadcast the message. Only {data}, {pres}, {info} are broadcastable.
	// {meta} and {ctrl} are sent to the session only
	for sess, pssd := range t.sessions {
		// Send all messages to multiplexing session.
		if !sess.isMultiplex() {
			if sess.sid == msg.SkipSid {
				continue
			}

			if msg.Pres != nil {
				// Skip notifying - already notified on topic.
				if msg.Pres.SkipTopic != "" && sess.getSub(msg.Pres.SkipTopic) != nil {
					continue
				}

				// Notification addressed to a single user only.
				if msg.Pres.SingleUser != "" && pssd.uid.UserId() != msg.Pres.SingleUser {
					continue
				}
				// Notification should skip a single user.
				if msg.Pres.ExcludeUser != "" && pssd.uid.UserId() == msg.Pres.ExcludeUser {
					continue
				}

				// Check presence filters
				if !t.passesPresenceFilters(msg.Pres, pssd.uid) {
					continue
				}

			} else {
				// Check if the user has Read permission or is a channel reader.
				if !t.userIsReader(pssd.uid) && !pssd.isChanSub {
					continue
				}

				// Don't send read receipts and key presses to channel readers.
				if msg.Info != nil && pssd.isChanSub {
					continue
				}

				// Don't send key presses from one user's session to the other sessions of the same user.
				if msg.Info != nil && msg.Info.What == "kp" && msg.Info.From == pssd.uid.UserId() {
					continue
				}
			}
		}

		// Topic name may be different depending on the user to which the `sess` belongs.
		t.maybeFixTopicName(msg, pssd.uid)

		// Send channel messages anonymously.
		if pssd.isChanSub && msg.Data != nil {
			msg.Data.From = ""
		}
		// Send message to session.
		if !sess.queueOut(msg) {
			log.Printf("topic[%s]: connection stuck, detaching - %s", t.name, sess.sid)
			// The whole session is being dropped, so sessionLeave.pkt is not set.
			// Must not block here: it may lead to a deadlock.
			select {
			case t.unreg <- &sessionLeave{sess: sess}:
			default:
				log.Printf("topic[%s]: unreg queue full - %s", t.name, sess.sid)
			}
		}
	}

	if !t.isProxy && pushRcpt != nil {
		// usersPush will update unread message count and send push notification.
		usersPush(pushRcpt)
	}
}

// subscriptionReply generates a response to a subscription request
func (t *Topic) subscriptionReply(h *Hub, asChan bool, join *sessionJoin) error {
	// The topic is already initialized by the Hub

	msgsub := join.pkt.Sub

	// For newly created topics report topic creation time.
	var now time.Time
	if msgsub.Created {
		now = t.updated
	} else {
		now = types.TimeNow()
	}

	asUid := types.ParseUserId(join.pkt.AsUser)

	if !msgsub.Newsub && (t.cat == types.TopicCatP2P || t.cat == types.TopicCatGrp || t.cat == types.TopicCatSys) {
		// Check if this is a new subscription.
		pud, found := t.perUser[asUid]
		msgsub.Newsub = !found || pud.deleted
	}

	var private interface{}
	var mode string
	if msgsub.Set != nil {
		if msgsub.Set.Sub != nil {
			if msgsub.Set.Sub.User != "" {
				join.sess.queueOut(ErrMalformedReply(join.pkt, now))
				return errors.New("user id must not be specified")
			}
			mode = msgsub.Set.Sub.Mode
		}

		if msgsub.Set.Desc != nil {
			private = msgsub.Set.Desc.Private
		}
	}

	var err error
	var modeChanged *MsgAccessMode
	// Create new subscription or modify an existing one.
	if modeChanged, err = t.thisUserSub(h, join.sess, join.pkt, asUid, mode, private); err != nil {
		return err
	}

	// Subscription successfully created. Link topic to session.
	join.sess.addSub(t.name, &Subscription{
		broadcast: t.broadcast,
		done:      t.unreg,
		meta:      t.meta,
		supd:      t.supd})
	t.addSession(join.sess, asUid, asChan)

	// The user is online in the topic. Increment the counter if notifications are not deferred.
	if !join.sess.background && !asChan {
		userData := t.perUser[asUid]
		userData.online++
		t.perUser[asUid] = userData
	}

	params := map[string]interface{}{}
	// Report back the assigned access mode.
	if modeChanged != nil {
		params["acs"] = modeChanged
	}
	toriginal := t.original(asUid)

	// When a group topic is created, it's given a temporary name by the client.
	// Then this name changes. Report back the original name here.
	if msgsub.Created && join.pkt.Original != toriginal {
		params["tmpname"] = join.pkt.Original
	}

	if len(params) == 0 {
		// Don't send empty params '{}'
		join.sess.queueOut(NoErr(join.pkt.Id, toriginal, now))
	} else {
		join.sess.queueOut(NoErrParams(join.pkt.Id, toriginal, now, params))
	}

	// Some notifications are always sent immediately.
	if modeChanged != nil {
		t.sendImmediateSubNotifications(asUid, modeChanged, join)
	}

	if !join.sess.background && !asChan {
		// Other notifications are also sent immediately for foreground sessions.
		t.sendSubNotifications(asUid, join.sess.sid, join.sess.userAgent)
	}

	return nil
}

// User requests or updates a self-subscription to a topic. Called as a
// result of {sub} or {meta set=sub}.
// Returns new access mode as *MsgAccessMode if user's access mode has changed, nil otherwise.
//
//	h				- hub
//	sess			- originating session
//	pkt				- client message which triggered this request (sub or set)
//	asUid			- id of the user making the request
//	want			- requested access mode
//	private			- private value to assign to the subscription
//	background		- presence notifications are deferred
//
// Handle these cases:
// A. User is trying to subscribe for the first time (no subscription).
// A.1 Reder is subscribeing to channel.
// A.2 Reader is joining the channel.
// B. User is already subscribed, just joining without changing anything.
// C. User is responding to an earlier invite (modeWant was "N" in subscription).
// D. User is already subscribed, changing modeWant.
// E. User is accepting ownership transfer (requesting ownership transfer is not permitted).
// In case of a group topic the user may be a reader or a full subscriber.
func (t *Topic) thisUserSub(h *Hub, sess *Session, pkt *ClientComMessage, asUid types.Uid, want string,
	private interface{}) (*MsgAccessMode, error) {

	now := types.TimeNow()

	asChan, err := t.verifyChannelAccess(pkt.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(pkt, now))
		return nil, types.ErrNotFound
	}

	asLvl := auth.Level(pkt.AuthLvl)

	// Access mode values as they were before this request was processed.
	oldWant := types.ModeNone
	oldGiven := types.ModeNone

	// Parse access mode requested by the user
	modeWant := types.ModeUnset
	if want != "" {
		if err := modeWant.UnmarshalText([]byte(want)); err != nil {
			sess.queueOut(ErrMalformedReply(pkt, now))
			return nil, err
		}
	}

	toriginal := t.original(asUid)

	// Check if it's an attempt at a new subscription to the topic / a channel reader (channel readers are not cached).
	// It could be an actual subscription (IsJoiner() == true) or a ban (IsJoiner() == false).
	userData, existingSub := t.perUser[asUid]
	if !existingSub || userData.deleted {
		// New subscription or a channel reader, either new or existing.

		// Check if the max number of subscriptions is already reached.
		if t.cat == types.TopicCatGrp && !asChan && t.subsCount() >= globals.maxSubscriberCount {
			sess.queueOut(ErrPolicyReply(pkt, now))
			return nil, errors.New("max subscription count exceeded")
		}

		var sub *types.Subscription
		tname := t.name
		if t.cat == types.TopicCatP2P {
			// P2P could be here only if it was previously deleted. I.e. existingSub is always true for P2P.
			if modeWant != types.ModeUnset {
				userData.modeWant = modeWant
			}
			// If no modeWant is provided, leave existing one unchanged.

			// Make sure the user is not asking for unreasonable permissions
			userData.modeWant = (userData.modeWant & types.ModeCP2P) | types.ModeApprove
		} else if t.cat == types.TopicCatSys {
			if asLvl != auth.LevelRoot {
				sess.queueOut(ErrPermissionDeniedReply(pkt, now))
				return nil, errors.New("subscription to 'sys' topic requires root access level")
			}

			// Assign default access levels
			userData.modeWant = types.ModeCSys
			userData.modeGiven = types.ModeCSys
			if modeWant != types.ModeUnset {
				userData.modeWant = (modeWant & types.ModeCSys) | types.ModeWrite | types.ModeJoin
			}
		} else if asChan {
			// Check if user is already subscribed.
			sub, err = store.Subs.Get(pkt.Original, asUid)
			if err != nil {
				sess.queueOut(ErrUnknown(pkt.Id, toriginal, now))
				return nil, err
			}

			// Given mode is immutable.
			oldGiven = types.ModeCChnReader
			userData.modeGiven = types.ModeCChnReader

			if sub != nil {
				// Subscription exists, read old access mode.
				oldWant = sub.ModeWant
			} else {
				// Subscription not found, use default.
				oldWant = types.ModeCChnReader
			}

			if modeWant != types.ModeUnset {
				// New access mode is explicitly assigned.
				userData.modeWant = (modeWant & types.ModeCChnReader) | types.ModeRead | types.ModeJoin
			} else {
				// Default: unchanged.
				userData.modeWant = oldWant
			}

			// User is subscribed to chnXXX, not grpXXX.
			tname = pkt.Original
		} else {
			// For all other topics access is given as default access.
			userData.modeGiven = t.accessFor(asLvl)

			if modeWant == types.ModeUnset {
				// User wants default access mode.
				userData.modeWant = userData.modeGiven
			} else {
				userData.modeWant = modeWant
			}
		}

		// Undelete.
		userData.deleted = false

		if isNullValue(private) {
			private = nil
		}
		userData.private = private

		// Add subscription to database, if missing.
		if sub == nil {
			sub = &types.Subscription{
				User:      asUid.String(),
				Topic:     tname,
				ModeWant:  userData.modeWant,
				ModeGiven: userData.modeGiven,
				Private:   userData.private,
			}

			if err := store.Subs.Create(sub); err != nil {
				sess.queueOut(ErrUnknownReply(pkt, now))
				return nil, err
			}

		} else if asChan && userData.modeWant != oldWant {
			// Channel reader changed access mode, save changed mode to db.
			if err := store.Subs.Update(tname, asUid,
				map[string]interface{}{"ModeWant": userData.modeWant}, true); err != nil {
				sess.queueOut(ErrUnknownReply(pkt, now))
				return nil, err
			}

			// Enable or disable fcm push notifications for the subsciption.
			t.channelSubUnsub(asUid, userData.modeWant.IsPresencer())
		}

		if asChan {
			if userData.modeWant != oldWant {
				pluginSubscription(sub, plgActCreate)
			} else {
				pluginSubscription(sub, plgActUpd)
			}
		} else {
			// Add subscribed user to cache.
			usersRegisterUser(asUid, true)
			// Notify plugins of a new subscription
			pluginSubscription(sub, plgActCreate)
		}

	} else {
		// Process update to existing subscription. It could be an incomplete subscription for a new topic.

		if asChan {
			// A normal subscriber is trying to access topic as a channel.
			// Direct the subscriber to use non-channel topic name.
			sess.queueOut(InfoUseOtherReply(pkt, t.name, now))
			return nil, types.ErrNotFound
		}

		var ownerChange bool

		// Save old access values

		oldWant = userData.modeWant
		oldGiven = userData.modeGiven

		if modeWant != types.ModeUnset {
			// Explicit modeWant is provided

			// Perform sanity checks
			if userData.modeGiven.IsOwner() {
				// Check for possible ownership transfer. Handle the following cases:
				// 1. Owner joining the topic without any changes
				// 2. Owner changing own settings
				// 3. Acceptance or rejection of the ownership transfer

				// Make sure the current owner cannot unset the owner flag or ban himself
				if t.owner == asUid && (!modeWant.IsOwner() || !modeWant.IsJoiner()) {
					sess.queueOut(ErrPermissionDeniedReply(pkt, now))
					return nil, errors.New("cannot unset ownership or self-ban the owner")
				}

				// Ownership transfer
				ownerChange = modeWant.IsOwner() && !userData.modeWant.IsOwner()

				// The owner should be able to grant himself any access permissions.
				// If ownership transfer is rejected don't upgrade.
				if modeWant.IsOwner() && !userData.modeGiven.BetterEqual(modeWant) {
					userData.modeGiven |= modeWant
				}
			} else if modeWant.IsOwner() {
				// Ownership transfer can only be initiated by the owner.
				sess.queueOut(ErrPermissionDeniedReply(pkt, now))
				return nil, errors.New("non-owner cannot request ownership transfer")
			} else if t.cat == types.TopicCatGrp && userData.modeGiven.IsAdmin() && modeWant.IsAdmin() {
				// A group topic Admin should be able to grant himself any permissions except
				// ownership (checked previously) & hard-deleting messages.
				if !userData.modeGiven.BetterEqual(modeWant & ^types.ModeDelete) {
					userData.modeGiven |= (modeWant & ^types.ModeDelete)
				}
			}

			if t.cat == types.TopicCatP2P {
				// For P2P topics ignore requests for 'D'. Otherwise it will generate a useless announcement.
				modeWant = (modeWant & types.ModeCP2P) | types.ModeApprove
			} else if t.cat == types.TopicCatSys {
				// Anyone can always write to Sys topic.
				modeWant &= (modeWant & types.ModeCSys) | types.ModeWrite
			}
		}

		// If user has not requested a new access mode, provide one by default.
		if modeWant == types.ModeUnset {
			// If the user has self-banned before, un-self-ban. Otherwise do not make a change.
			if !oldWant.IsJoiner() {
				// Set permissions NO WORSE than default, but possibly better (admin or owner banned himself).
				userData.modeWant = userData.modeGiven | t.accessFor(asLvl)
			}
		} else if userData.modeWant != modeWant {
			// The user has provided a new modeWant and it' different from the one before
			userData.modeWant = modeWant
		}

		// Save changes to DB
		update := map[string]interface{}{}
		if isNullValue(private) {
			update["Private"] = nil
			userData.private = nil
		} else if private != nil {
			update["Private"] = private
			userData.private = private
		}
		if userData.modeWant != oldWant {
			update["ModeWant"] = userData.modeWant
		}
		if userData.modeGiven != oldGiven {
			update["ModeGiven"] = userData.modeGiven
		}
		if len(update) > 0 {
			if err := store.Subs.Update(t.name, asUid, update, true); err != nil {
				sess.queueOut(ErrUnknownReply(pkt, now))
				return nil, err
			}
		}

		// No transactions in RethinkDB, but two owners are better than none
		if ownerChange {
			oldOwnerData := t.perUser[t.owner]
			oldOwnerOldWant, oldOwnerOldGiven := oldOwnerData.modeWant, oldOwnerData.modeGiven
			oldOwnerData.modeGiven = (oldOwnerData.modeGiven & ^types.ModeOwner)
			oldOwnerData.modeWant = (oldOwnerData.modeWant & ^types.ModeOwner)
			if err := store.Subs.Update(t.name, t.owner,
				map[string]interface{}{
					"ModeWant":  oldOwnerData.modeWant,
					"ModeGiven": oldOwnerData.modeGiven}, false); err != nil {
				return nil, err
			}
			if err := store.Topics.OwnerChange(t.name, asUid); err != nil {
				return nil, err
			}
			t.perUser[t.owner] = oldOwnerData
			// Send presence notifications.
			t.notifySubChange(t.owner, asUid, false,
				oldOwnerOldWant, oldOwnerOldGiven, oldOwnerData.modeWant, oldOwnerData.modeGiven, "")
			t.owner = asUid
		}
	}

	if !asChan {
		// If topic is being muted, send "off" notification and disable updates.
		// Do it before applying the new permissions.
		if (oldWant & oldGiven).IsPresencer() && !(userData.modeWant & userData.modeGiven).IsPresencer() {
			if t.cat == types.TopicCatMe {
				t.presUsersOfInterest("off+dis", t.userAgent)
			} else {
				t.presSingleUserOffline(asUid, userData.modeWant&userData.modeGiven,
					"off+dis", nilPresParams, "", false)
			}
		}

		// Apply changes.
		t.perUser[asUid] = userData
	}

	var modeChanged *MsgAccessMode
	// Send presence notifications and update cached unread count.
	if oldWant != userData.modeWant || oldGiven != userData.modeGiven {
		if !asChan {
			oldReader := (oldWant & oldGiven).IsReader()
			newReader := (userData.modeWant & userData.modeGiven).IsReader()

			if oldReader && !newReader {
				// Decrement unread count
				usersUpdateUnread(asUid, userData.readID-t.lastID, true)
			} else if !oldReader && newReader {
				// Increment unread count
				usersUpdateUnread(asUid, t.lastID-userData.readID, true)
			}
		}

		// Notify user of the changes in access mode.
		t.notifySubChange(asUid, asUid, asChan, oldWant, oldGiven, userData.modeWant, userData.modeGiven, sess.sid)

		modeChanged = &MsgAccessMode{
			Want:  userData.modeWant.String(),
			Given: userData.modeGiven.String(),
			Mode:  (userData.modeGiven & userData.modeWant).String(),
		}
	}

	if !userData.modeWant.IsJoiner() {
		// The user is self-banning from the topic. Re-subscription will unban.
		t.evictUser(asUid, false, "")
		// The callee will send NoErrOK
		return modeChanged, nil

	} else if !userData.modeGiven.IsJoiner() {
		// User was banned
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("topic access denied; user is banned")
	}

	return modeChanged, nil
}

// anotherUserSub processes a request to initiate an invite or approve a subscription request from another user.
// Returns changed == true if user's access mode has changed.
// Handle these cases:
// A. Sharer or Approver is inviting another user for the first time (no prior subscription)
// B. Sharer or Approver is re-inviting another user (adjusting modeGiven, modeWant is still Unset)
// C. Approver is changing modeGiven for another user, modeWant != Unset
func (t *Topic) anotherUserSub(h *Hub, sess *Session, asUid, target types.Uid,
	pkt *ClientComMessage) (*MsgAccessMode, error) {

	now := types.TimeNow()
	set := pkt.Set

	// Access mode values as they were before this request was processed.
	oldWant := types.ModeUnset
	oldGiven := types.ModeUnset

	// Access mode of the person who is executing this approval process
	var hostMode types.AccessMode

	// Check if approver actually has permission to manage sharing
	userData, ok := t.perUser[asUid]
	if !ok || !(userData.modeGiven & userData.modeWant).IsSharer() {
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("topic access denied; approver has no permission")
	}

	asChan, err := t.verifyChannelAccess(pkt.Original)
	if asChan {
		// TODO: need to implement promoting reader to subscriber.
		// Just reject for now.
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("topic access denied: cannot subscribe reader to channel")
	} else if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(pkt, now))
		return nil, types.ErrNotFound
	}

	// Check if topic is suspended.
	if t.isReadOnly() {
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("topic is suspended")
	}

	hostMode = userData.modeGiven & userData.modeWant

	// Parse the access mode granted
	modeGiven := types.ModeUnset
	if set.Sub.Mode != "" {
		if err := modeGiven.UnmarshalText([]byte(set.Sub.Mode)); err != nil {
			sess.queueOut(ErrMalformedReply(pkt, now))
			return nil, err
		}

		// Make sure the new permissions are reasonable in P2P topics: permissions no greater than default,
		// approver permission cannot be removed.
		if t.cat == types.TopicCatP2P {
			modeGiven = (modeGiven & types.ModeCP2P) | types.ModeApprove
		}
	}

	// Make sure only the owner & approvers can set non-default access mode
	if modeGiven != types.ModeUnset && !hostMode.IsAdmin() {
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("sharer cannot set explicit modeGiven")
	}

	// Make sure no one but the owner can do an ownership transfer
	if modeGiven.IsOwner() && t.owner != asUid {
		sess.queueOut(ErrPermissionDeniedReply(pkt, now))
		return nil, errors.New("attempt to transfer ownership by non-owner")
	}

	// Check if it's a new invite. If so, save it to database as a subscription.
	// Saved subscription does not mean the user is allowed to post/read
	userData, existingSub := t.perUser[target]
	if !existingSub {
		// Check if the max number of subscriptions is already reached.
		if t.cat == types.TopicCatGrp && t.subsCount() >= globals.maxSubscriberCount {
			sess.queueOut(ErrPolicyReply(pkt, now))
			return nil, errors.New("max subscription count exceeded")
		}

		if modeGiven == types.ModeUnset {
			// Request to use default access mode for the new subscriptions.
			// Assuming LevelAuth. Approver should use non-default access if that is not suitable.
			modeGiven = t.accessFor(auth.LevelAuth)
			// Enable new subscription even if default is no joiner.
			modeGiven |= types.ModeJoin
		}

		// Get user's default access mode to be used as modeWant
		var modeWant types.AccessMode
		if user, err := store.Users.Get(target); err != nil {
			sess.queueOut(ErrUnknownReply(pkt, now))
			return nil, err
		} else if user == nil {
			sess.queueOut(ErrUserNotFoundReply(pkt, now))
			return nil, errors.New("user not found")
		} else if user.State != types.StateOK {
			sess.queueOut(ErrPermissionDeniedReply(pkt, now))
			return nil, errors.New("user is suspended")
		} else {
			// Don't ask by default for more permissions than the granted ones.
			modeWant = user.Access.Auth & modeGiven
		}

		// Add subscription to database
		sub := &types.Subscription{
			User:      target.String(),
			Topic:     t.name,
			ModeWant:  modeWant,
			ModeGiven: modeGiven,
		}

		if err := store.Subs.Create(sub); err != nil {
			sess.queueOut(ErrUnknownReply(pkt, now))
			return nil, err
		}

		userData = perUserData{
			modeGiven: sub.ModeGiven,
			modeWant:  sub.ModeWant,
			private:   nil,
		}
		t.perUser[target] = userData
		t.computePerUserAcsUnion()

		// Cache user's record
		usersRegisterUser(target, true)

		// Send push notification for the new subscription.
		if pushRcpt := t.pushForSub(asUid, target, userData.modeWant, userData.modeGiven, now); pushRcpt != nil {
			// TODO: maybe skip user's devices which were online when this event has happened.
			usersPush(pushRcpt)
		}
	} else {
		// Action on an existing subscription: re-invite, change existing permission, confirm/decline request.
		oldGiven = userData.modeGiven
		oldWant = userData.modeWant

		if modeGiven == types.ModeUnset {
			// Request to re-send invite without changing the access mode
			modeGiven = userData.modeGiven
		} else if modeGiven != userData.modeGiven {
			// Changing the previously assigned value
			userData.modeGiven = modeGiven

			// Save changed value to database
			if err := store.Subs.Update(t.name, target,
				map[string]interface{}{"ModeGiven": modeGiven}, false); err != nil {
				return nil, err
			}
			t.perUser[target] = userData
		}
	}

	var modeChanged *MsgAccessMode
	// Access mode has changed.
	if oldGiven != userData.modeGiven {

		oldReader := (oldWant & oldGiven).IsReader()
		newReader := (userData.modeWant & userData.modeGiven).IsReader()
		if oldReader && !newReader {
			// Decrement unread count
			usersUpdateUnread(target, userData.readID-t.lastID, true)
		} else if !oldReader && newReader {
			// Increment unread count
			usersUpdateUnread(target, t.lastID-userData.readID, true)
		}
		t.notifySubChange(target, asUid, false,
			oldWant, oldGiven, userData.modeWant, userData.modeGiven, sess.sid)

		modeChanged = &MsgAccessMode{
			Given: userData.modeGiven.String(),
			Want:  userData.modeWant.String(),
			Mode:  (userData.modeGiven & userData.modeWant).String(),
		}
	}

	if !userData.modeGiven.IsJoiner() {
		// The user is banned from the topic.
		t.evictUser(target, false, "")
	}

	return modeChanged, nil
}

// replyGetDesc is a response to a get.desc request on a topic, sent to just the session as a {meta} packet
func (t *Topic) replyGetDesc(sess *Session, asUid types.Uid, opts *MsgGetOpts, msg *ClientComMessage) error {
	now := types.TimeNow()
	id := msg.Id

	if opts != nil && (opts.User != "" || opts.Limit != 0) {
		sess.queueOut(ErrMalformedReply(msg, now))
		return errors.New("invalid GetDesc query")
	}

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}

	// Check if user requested modified data
	ifUpdated := opts == nil || opts.IfModifiedSince == nil || opts.IfModifiedSince.Before(t.updated)

	desc := &MsgTopicDesc{}
	if !ifUpdated {
		desc.CreatedAt = &t.created
	}
	if !t.updated.IsZero() {
		desc.UpdatedAt = &t.updated
	}

	pud, full := t.perUser[asUid]

	if t.cat == types.TopicCatMe {
		full = true
	}

	if ifUpdated {
		if t.public != nil {
			desc.Public = t.public
		} else if full && t.cat == types.TopicCatP2P {
			desc.Public = pud.public
		}
	}

	// Request may come from a subscriber (full == true) or a stranger.
	// Give subscriber a fuller description than to a stranger
	if full {
		if t.cat == types.TopicCatP2P {
			// For p2p topics default access mode makes no sense.
			// Don't report it.
		} else if t.cat == types.TopicCatMe || (pud.modeGiven & pud.modeWant).IsSharer() {
			desc.DefaultAcs = &MsgDefaultAcsMode{
				Auth: t.accessAuth.String(),
				Anon: t.accessAnon.String()}
		}

		desc.Acs = &MsgAccessMode{
			Want:  pud.modeWant.String(),
			Given: pud.modeGiven.String(),
			Mode:  (pud.modeGiven & pud.modeWant).String()}

		if t.cat == types.TopicCatMe && sess.authLvl == auth.LevelRoot {
			// If 'me' is in memory then user account is invariably not suspended.
			desc.State = types.StateOK.String()
		}

		if t.cat == types.TopicCatGrp && (pud.modeGiven & pud.modeWant).IsPresencer() {
			desc.Online = t.isOnline()
		}
		if ifUpdated {
			desc.Private = pud.private
		}

		// Don't report message IDs to users without Read access.
		if (pud.modeGiven & pud.modeWant).IsReader() {
			desc.SeqId = t.lastID
			if !t.touched.IsZero() {
				desc.TouchedAt = &t.touched
			}

			// Make sure reported values are sane:
			// t.delID <= pud.delID; t.readID <= t.recvID <= t.lastID
			desc.DelId = max(pud.delID, t.delID)
			desc.ReadSeqId = pud.readID
			desc.RecvSeqId = max(pud.recvID, pud.readID)
		} else {
			// Send some sane value of touched.
			desc.TouchedAt = &t.updated
		}
	} else if asChan {
		desc.SeqId = t.lastID
		if !t.touched.IsZero() {
			desc.TouchedAt = &t.touched
		}
		// Fetch subscription data from DB for channel readers.
		sub, _ := store.Subs.Get(msg.Original, asUid)
		// Ignoring the error: it's not useful here.
		if sub != nil {
			desc.Acs = &MsgAccessMode{
				Want:  sub.ModeWant.String(),
				Given: sub.ModeWant.String(),
				Mode:  (sub.ModeGiven & sub.ModeWant).String()}
			if ifUpdated {
				desc.Private = sub.Private
			}
			desc.DelId = max(sub.DelId, t.delID)
			desc.ReadSeqId = sub.ReadSeqId
			desc.RecvSeqId = max(sub.RecvSeqId, sub.ReadSeqId)
		}
	}

	sess.queueOut(&ServerComMessage{
		Meta: &MsgServerMeta{
			Id:        id,
			Topic:     msg.Original,
			Desc:      desc,
			Timestamp: &now}})

	return nil
}

// replySetDesc updates topic metadata, saves it to DB,
// replies to the caller as {ctrl} message, generates {pres} update if necessary
func (t *Topic) replySetDesc(sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	now := types.TimeNow()
	set := msg.Set

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}

	assignAccess := func(upd map[string]interface{}, mode *MsgDefaultAcsMode) error {
		if mode == nil {
			return nil
		}
		if auth, anon, err := parseTopicAccess(mode, types.ModeUnset, types.ModeUnset); err != nil {
			return err
		} else if auth.IsOwner() || anon.IsOwner() {
			return errors.New("default 'owner' access is not permitted")
		} else {
			access := types.DefaultAccess{Auth: t.accessAuth, Anon: t.accessAnon}
			if auth != types.ModeUnset {
				if t.cat == types.TopicCatMe {
					auth &= types.ModeCAuth
					if auth != types.ModeNone {
						// This is the default access mode for P2P topics.
						// It must be either an N or must include an A permission.
						auth |= types.ModeApprove
					}
				}
				access.Auth = auth
			}
			if anon != types.ModeUnset {
				if t.cat == types.TopicCatMe {
					anon &= types.ModeCP2P
					if anon != types.ModeNone {
						anon |= types.ModeApprove
					}
				}
				access.Anon = anon
			}
			if access.Auth != t.accessAuth || access.Anon != t.accessAnon {
				upd["Access"] = access
			}
		}
		return nil
	}

	assignGenericValues := func(upd map[string]interface{}, what string, dst, src interface{}) (changed bool) {
		if dst, changed = mergeInterfaces(dst, src); changed {
			upd[what] = dst
		}
		return
	}

	// DefaultAccess and/or Public have chanegd
	var sendCommon bool
	// Private has changed
	var sendPriv bool

	// Change to the main object (user or topic).
	core := make(map[string]interface{})
	// Change to subscription.
	sub := make(map[string]interface{})
	if set.Desc != nil {
		switch t.cat {
		case types.TopicCatMe:
			// Update current user
			err = assignAccess(core, set.Desc.DefaultAcs)
			sendCommon = assignGenericValues(core, "Public", t.public, set.Desc.Public)
		case types.TopicCatFnd:
			// set.Desc.DefaultAcs is ignored.
			// Do not send presence if fnd.Public has changed.
			assignGenericValues(core, "Public", t.fndGetPublic(sess), set.Desc.Public)
		case types.TopicCatP2P:
			// Reject direct changes to P2P topics.
			if set.Desc.Public != nil || set.Desc.DefaultAcs != nil {
				sess.queueOut(ErrPermissionDeniedReply(msg, now))
				return errors.New("incorrect attempt to change metadata of a p2p topic")
			}
		case types.TopicCatGrp:
			// Update group topic
			if t.owner == asUid {
				err = assignAccess(core, set.Desc.DefaultAcs)
				sendCommon = assignGenericValues(core, "Public", t.public, set.Desc.Public)
			} else if set.Desc.DefaultAcs != nil || set.Desc.Public != nil {
				// This is a request from non-owner
				sess.queueOut(ErrPermissionDeniedReply(msg, now))
				return errors.New("attempt to change public or permissions by non-owner")
			}
		}

		if err != nil {
			sess.queueOut(ErrMalformedReply(msg, now))
			return err
		}

		sendPriv = assignGenericValues(sub, "Private", t.perUser[asUid].private, set.Desc.Private)
	}

	if len(core)+len(sub) == 0 {
		sess.queueOut(InfoNotModifiedReply(msg, now))
		return errors.New("{set} generated no update to DB")
	}

	if len(core) > 0 {
		core["UpdatedAt"] = now
		switch t.cat {
		case types.TopicCatMe:
			err = store.Users.Update(asUid, core)
		case types.TopicCatFnd:
			// The only value to be stored in topic is Public, and Public for fnd is not saved according to specs.
		default:
			err = store.Topics.Update(t.name, core)
		}
	}
	if err == nil && len(sub) > 0 {
		tname := t.name
		if asChan {
			tname = types.GrpToChn(tname)
		}
		err = store.Subs.Update(tname, asUid, sub, true)
	}

	if err != nil {
		sess.queueOut(ErrUnknownReply(msg, now))
		return err
	}

	// Update values cached in the topic object
	if t.cat == types.TopicCatMe || t.cat == types.TopicCatGrp {
		if tmp, ok := core["Access"]; ok {
			access := tmp.(types.DefaultAccess)
			t.accessAuth = access.Auth
			t.accessAnon = access.Anon
		}
		if public, ok := core["Public"]; ok {
			t.public = public
		}
	} else if t.cat == types.TopicCatFnd {
		// Assign per-session fnd.Public.
		t.fndSetPublic(sess, core["Public"])
	}

	mode := types.ModeNone
	if private, ok := sub["Private"]; ok && !asChan {
		pud := t.perUser[asUid]
		pud.private = private
		pud.updated = now
		t.perUser[asUid] = pud
		mode = pud.modeGiven & pud.modeWant
	}

	if sendCommon || sendPriv {
		// t.public, t.accessAuth/Anon have changed, make an announcement
		if sendCommon {
			if t.cat == types.TopicCatMe {
				t.presUsersOfInterest("upd", "")
			} else {
				// Notify all subscribers on 'me' except the user who made the change and blocked users.
				// The user who made the change will be notified separately (see below).
				filter := &presFilters{excludeUser: asUid.UserId(), filterIn: types.ModeJoin}
				t.presSubsOffline("upd", nilPresParams, filter, filter, sess.sid, false)
			}

			t.updated = now
		}
		// Notify user's other sessions.
		t.presSingleUserOffline(asUid, mode, "upd", nilPresParams, sess.sid, false)
	}

	sess.queueOut(NoErrReply(msg, now))

	return nil
}

// replyGetSub is a response to a get.sub request on a topic - load a list of subscriptions/subscribers,
// send it just to the session as a {meta} packet
func (t *Topic) replyGetSub(sess *Session, asUid types.Uid, authLevel auth.Level, msg *ClientComMessage) error {
	now := types.TimeNow()
	id := msg.Id
	incomingReqTs := msg.Timestamp
	var req *MsgGetOpts
	if msg.Sub != nil {
		req = msg.Sub.Get.Sub
	} else {
		req = msg.Get.Sub
	}

	if req != nil && (req.SinceId != 0 || req.BeforeId != 0) {
		sess.queueOut(ErrMalformedReply(msg, now))
		return errors.New("invalid MsgGetOpts query")
	}

	if _, err := t.verifyChannelAccess(msg.Original); err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}

	userData := t.perUser[asUid]
	if !(userData.modeGiven & userData.modeWant).IsSharer() {
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return errors.New("user does not have S permission")
	}

	var ifModified time.Time
	if req != nil && req.IfModifiedSince != nil {
		ifModified = *req.IfModifiedSince
	}

	var subs []types.Subscription
	var err error

	switch t.cat {
	case types.TopicCatMe:
		if req != nil {
			// If topic is provided, it could be in the form of user ID 'usrAbCd'.
			// Convert it to P2P topic name.
			if uid2 := types.ParseUserId(req.Topic); !uid2.IsZero() {
				req.Topic = uid2.P2PName(asUid)
			}
		}
		// Fetch user's subscriptions, with Topic.Public denormalized into subscription.
		if ifModified.IsZero() {
			// No cache management. Skip deleted subscriptions.
			subs, err = store.Users.GetTopics(asUid, msgOpts2storeOpts(req))
		} else {
			// User manages cache. Include deleted subscriptions too.
			subs, err = store.Users.GetTopicsAny(asUid, msgOpts2storeOpts(req))
		}
	case types.TopicCatFnd:
		// Select public or private query. Public has priority.
		rewriteLogin := true
		raw := t.fndGetPublic(sess)
		if raw == nil {
			rewriteLogin = false
			raw = userData.private
		}

		if query, ok := raw.(string); ok && len(query) > 0 {
			query, subs, err = pluginFind(asUid, query)
			if err == nil && subs == nil && query != "" {
				var req [][]string
				var opt []string
				if req, opt, err = parseSearchQuery(query, sess.countryCode, rewriteLogin); err == nil {
					if len(req) > 0 || len(opt) > 0 {
						// Check if the query contains terms that the user is not allowed to use.
						allReq := types.FlattenDoubleSlice(req)
						restr, _ := stringSliceDelta(t.tags, filterRestrictedTags(append(allReq, opt...),
							globals.maskedTagNS))

						if len(restr) > 0 {
							sess.queueOut(ErrPermissionDeniedReply(msg, now))
							return errors.New("attempt to search by restricted tags")
						}

						// FIXME: allow root to find suspended users and topics.
						subs, err = store.Users.FindSubs(asUid, req, opt)
						if err != nil {
							sess.queueOut(decodeStoreErrorExplicitTs(err, id, t.original(asUid), now, incomingReqTs, nil))
							return err
						}

					} else {
						// Query string is empty.
						sess.queueOut(ErrMalformedReply(msg, now))
						return errors.New("empty search query")
					}
				} else {
					// Query parsing error. Report it externally as a generic ErrMalformed.
					sess.queueOut(ErrMalformedReply(msg, now))
					return errors.New("failed to parse search query; " + err.Error())
				}
			}
		}
	case types.TopicCatP2P:
		// FIXME(gene): don't load subs from DB, use perUserData - it already contains subscriptions.
		// No need to load Public for p2p topics.
		if ifModified.IsZero() {
			// No cache management. Skip deleted subscriptions.
			subs, err = store.Topics.GetSubs(t.name, msgOpts2storeOpts(req))
		} else {
			// User manages cache. Include deleted subscriptions too.
			subs, err = store.Topics.GetSubsAny(t.name, msgOpts2storeOpts(req))
		}
	case types.TopicCatGrp:
		// Include sub.Public.
		if ifModified.IsZero() {
			// No cache management. Skip deleted subscriptions.
			subs, err = store.Topics.GetUsers(t.name, msgOpts2storeOpts(req))
		} else {
			// User manages cache. Include deleted subscriptions too.
			subs, err = store.Topics.GetUsersAny(t.name, msgOpts2storeOpts(req))
		}
	}

	if err != nil {
		sess.queueOut(decodeStoreErrorExplicitTs(err, id, t.original(asUid), now, incomingReqTs, nil))
		return err
	}

	if len(subs) > 0 {
		meta := &MsgServerMeta{Id: id, Topic: t.original(asUid), Timestamp: &now}
		meta.Sub = make([]MsgTopicSub, 0, len(subs))
		presencer := (userData.modeGiven & userData.modeWant).IsPresencer()

		for i := range subs {
			sub := &subs[i]
			// Indicator if the requester has provided a cut off date for ts of pub & priv updates.
			var sendPubPriv bool
			var banned bool
			var mts MsgTopicSub
			deleted := sub.DeletedAt != nil

			if ifModified.IsZero() {
				sendPubPriv = true
			} else {
				// Skip sending deleted subscriptions if they were deleted before the cut off date.
				// If they are freshly deleted send minimum info
				if deleted {
					if !sub.DeletedAt.After(ifModified) {
						continue
					}
					mts.DeletedAt = sub.DeletedAt
				}
				sendPubPriv = !deleted && sub.UpdatedAt.After(ifModified)
			}

			uid := types.ParseUid(sub.User)
			isReader := (sub.ModeGiven & sub.ModeWant).IsReader()
			if t.cat == types.TopicCatMe {
				// Mark subscriptions that the user does not care about.
				if !(sub.ModeWant & sub.ModeGiven).IsJoiner() {
					banned = true
				}

				// Reporting user's subscriptions to other topics. P2P topic name is the
				// UID of the other user.
				with := sub.GetWith()
				if with != "" {
					mts.Topic = with
					mts.Online = t.perSubs[with].online && !deleted && presencer
				} else {
					mts.Topic = sub.Topic
					mts.Online = t.perSubs[sub.Topic].online && !deleted && presencer
				}

				if !deleted && !banned {
					if isReader {
						if sub.GetTouchedAt().IsZero() {
							mts.TouchedAt = nil
						} else {
							touchedAt := sub.GetTouchedAt()
							mts.TouchedAt = &touchedAt
						}
						mts.SeqId = sub.GetSeqId()
						mts.DelId = sub.DelId
					} else {
						mts.TouchedAt = &sub.UpdatedAt
					}

					lastSeen := sub.GetLastSeen()
					if !lastSeen.IsZero() && !mts.Online {
						mts.LastSeen = &MsgLastSeenInfo{
							When:      &lastSeen,
							UserAgent: sub.GetUserAgent()}
					}
				}
			} else {
				// Mark subscriptions that the user does not care about.
				if t.cat == types.TopicCatGrp && !(sub.ModeWant & sub.ModeGiven).IsJoiner() {
					banned = true
				}

				// Reporting subscribers to fnd, a group or a p2p topic
				mts.User = uid.UserId()
				if t.cat == types.TopicCatFnd {
					mts.Topic = sub.Topic
				}

				if !deleted {
					if uid == asUid && isReader && !banned {
						// Report deleted ID for own subscriptions only
						mts.DelId = sub.DelId
					}

					if t.cat == types.TopicCatGrp {
						pud := t.perUser[uid]
						mts.Online = pud.online > 0 && presencer
					}
				}
			}

			if !deleted {
				mts.UpdatedAt = &sub.UpdatedAt
				if isReader && !banned {
					mts.ReadSeqId = sub.ReadSeqId
					mts.RecvSeqId = sub.RecvSeqId
				}

				if t.cat != types.TopicCatFnd {
					mts.Acs.Mode = (sub.ModeGiven & sub.ModeWant).String()
					mts.Acs.Want = sub.ModeWant.String()
					mts.Acs.Given = sub.ModeGiven.String()
				} else {
					// Topic 'fnd'
					// sub.ModeXXX may be defined by the plugin.
					if sub.ModeGiven.IsDefined() && sub.ModeWant.IsDefined() {
						mts.Acs.Mode = (sub.ModeGiven & sub.ModeWant).String()
						mts.Acs.Want = sub.ModeWant.String()
						mts.Acs.Given = sub.ModeGiven.String()
					} else if isChannel(sub.Topic) {
						mts.Acs.Mode = types.ModeCChnReader.String()
					} else if defacs := sub.GetDefaultAccess(); defacs != nil {
						switch authLevel {
						case auth.LevelAnon:
							mts.Acs.Mode = defacs.Anon.String()
						case auth.LevelAuth, auth.LevelRoot:
							mts.Acs.Mode = defacs.Auth.String()
						}
					}
				}

				// Returning public and private only if they have changed since ifModified
				if sendPubPriv {
					// 'sub' has nil 'public' in p2p topics which is OK.
					mts.Public = sub.GetPublic()
					// Reporting 'private' only if it's user's own subscription.
					if uid == asUid {
						mts.Private = sub.Private
					}
				}

				// Always reporting 'private' for fnd topic.
				if t.cat == types.TopicCatFnd {
					mts.Private = sub.Private
				}
			}

			meta.Sub = append(meta.Sub, mts)
		}
		sess.queueOut(&ServerComMessage{Meta: meta})
	} else {
		// Inform the client that there are no subscriptions.
		sess.queueOut(NoContentParamsReply(msg, now, map[string]interface{}{"what": "sub"}))
	}

	return nil
}

// replySetSub is a response to new subscription request or an update to a subscription {set.sub}:
// update topic metadata cache, save/update subs, reply to the caller as {ctrl} message,
// generate a presence notification, if appropriate.
func (t *Topic) replySetSub(h *Hub, sess *Session, pkt *ClientComMessage) error {
	now := types.TimeNow()

	asUid := types.ParseUserId(pkt.AsUser)
	set := pkt.Set

	if _, err := t.verifyChannelAccess(pkt.Original); err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(pkt, now))
		return types.ErrNotFound
	}

	var target types.Uid
	if target = types.ParseUserId(set.Sub.User); target.IsZero() && set.Sub.User != "" {
		// Invalid user ID
		sess.queueOut(ErrMalformedReply(pkt, now))
		return errors.New("invalid user id")
	}

	// if set.User is not set, request is for the current user
	if target.IsZero() {
		target = asUid
	}

	var err error
	var modeChanged *MsgAccessMode
	if target == asUid {
		// Request new subscription or modify own subscription
		modeChanged, err = t.thisUserSub(h, sess, pkt, asUid, set.Sub.Mode, nil)
	} else {
		// Request to approve/change someone's subscription
		modeChanged, err = t.anotherUserSub(h, sess, asUid, target, pkt)
	}
	if err != nil {
		return err
	}

	var resp *ServerComMessage
	if modeChanged != nil {
		// Report resulting access mode.
		params := map[string]interface{}{"acs": modeChanged}
		if target != asUid {
			params["user"] = target.UserId()
		}
		resp = NoErrParamsReply(pkt, now, params)
	} else {
		resp = InfoNotModifiedReply(pkt, now)
	}

	sess.queueOut(resp)

	return nil
}

// replyGetData is a response to a get.data request - load a list of stored messages, send them to session as {data}
// response goes to a single session rather than all sessions in a topic
func (t *Topic) replyGetData(sess *Session, asUid types.Uid, req *MsgGetOpts, msg *ClientComMessage) error {
	now := types.TimeNow()
	toriginal := t.original(asUid)

	if req != nil && (req.IfModifiedSince != nil || req.User != "" || req.Topic != "") {
		sess.queueOut(ErrMalformedReply(msg, now))
		return errors.New("invalid MsgGetOpts query")
	}

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}

	// Check if the user has permission to read the topic data
	count := 0
	if userData := t.perUser[asUid]; (userData.modeGiven & userData.modeWant).IsReader() || asChan {
		// Read messages from DB
		messages, err := store.Messages.GetAll(t.name, asUid, msgOpts2storeOpts(req))
		if err != nil {
			sess.queueOut(ErrUnknownReply(msg, now))
			return err
		}

		// Push the list of messages to the client as {data}.
		if messages != nil {
			count = len(messages)
			for i := range messages {
				mm := &messages[i]
				from := ""
				if !asChan {
					// Don't show sender for channel readers
					from = types.ParseUid(mm.From).UserId()
				}
				sess.queueOut(&ServerComMessage{Data: &MsgServerData{
					Topic:     toriginal,
					Head:      mm.Head,
					SeqId:     mm.SeqId,
					From:      from,
					Timestamp: mm.CreatedAt,
					Content:   mm.Content}})
			}
		}
	}

	// Inform the requester that all the data has been served.
	if count == 0 {
		sess.queueOut(NoContentParamsReply(msg, now, map[string]interface{}{"what": "data"}))
	} else {
		sess.queueOut(NoErrDeliveredParams(msg.Id, msg.Original, now,
			map[string]interface{}{"what": "data", "count": count}))
	}

	return nil
}

// replyGetTags returns topic's tags - tokens used for discovery.
func (t *Topic) replyGetTags(sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	now := types.TimeNow()

	if _, err := t.verifyChannelAccess(msg.Original); err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}
	if t.cat != types.TopicCatMe && t.cat != types.TopicCatGrp {
		sess.queueOut(ErrOperationNotAllowedReply(msg, now))
		return errors.New("invalid topic category for getting tags")
	}
	if t.cat == types.TopicCatGrp && t.owner != asUid {
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return errors.New("request for tags from non-owner")
	}

	if len(t.tags) > 0 {
		sess.queueOut(&ServerComMessage{
			Meta: &MsgServerMeta{Id: msg.Id, Topic: t.original(asUid), Timestamp: &now, Tags: t.tags}})
		return nil
	}

	// Inform the requester that there are no tags.
	sess.queueOut(NoContentParamsReply(msg, now, map[string]string{"what": "tags"}))

	return nil
}

// replySetTags updates topic's tags - tokens used for discovery.
func (t *Topic) replySetTags(sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	var resp *ServerComMessage
	var err error
	set := msg.Set

	now := types.TimeNow()

	if _, err := t.verifyChannelAccess(msg.Original); err != nil {
		// User should not be able to address non-channel topic as channel.
		resp = ErrNotFoundReply(msg, now)
		err = types.ErrNotFound

	} else if t.cat != types.TopicCatMe && t.cat != types.TopicCatGrp {
		resp = ErrOperationNotAllowedReply(msg, now)
		err = errors.New("invalid topic category to assign tags")

	} else if t.cat == types.TopicCatGrp && t.owner != asUid {
		resp = ErrPermissionDeniedReply(msg, now)
		err = errors.New("tags update by non-owner")

	} else if tags := normalizeTags(set.Tags); tags != nil {
		if !restrictedTagsEqual(t.tags, tags, globals.immutableTagNS) {
			err = errors.New("attempt to mutate restricted tags")
			resp = ErrPermissionDeniedReply(msg, now)
		} else {
			added, removed := stringSliceDelta(t.tags, tags)
			if len(added) > 0 || len(removed) > 0 {
				update := map[string]interface{}{"Tags": types.StringSlice(tags), "UpdatedAt": now}
				if t.cat == types.TopicCatMe {
					err = store.Users.Update(asUid, update)
				} else if t.cat == types.TopicCatGrp {
					err = store.Topics.Update(t.name, update)
				}

				if err != nil {
					resp = ErrUnknownReply(msg, now)
				} else {
					t.tags = tags
					t.presSubsOnline("tags", "", nilPresParams, &presFilters{singleUser: asUid.UserId()}, "")

					params := make(map[string]interface{})
					if len(added) > 0 {
						params["added"] = len(added)
					}
					if len(removed) > 0 {
						params["removed"] = len(removed)
					}
					resp = NoErrParamsReply(msg, now, params)
				}
			} else {
				resp = InfoNotModifiedReply(msg, now)
			}
		}
	} else {
		resp = InfoNotModifiedReply(msg, now)
	}

	sess.queueOut(resp)

	return err
}

// replyGetCreds returns user's credentials such as email and phone numbers.
func (t *Topic) replyGetCreds(sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	now := types.TimeNow()
	id := msg.Id

	if t.cat != types.TopicCatMe {
		sess.queueOut(ErrOperationNotAllowedReply(msg, now))
		return errors.New("invalid topic category for getting credentials")
	}

	screds, err := store.Users.GetAllCreds(asUid, "", false)
	if err != nil {
		sess.queueOut(decodeStoreErrorExplicitTs(err, id, msg.Original, now, msg.Timestamp, nil))
		return err
	}

	if len(screds) > 0 {
		creds := make([]*MsgCredServer, len(screds))
		for i, sc := range screds {
			creds[i] = &MsgCredServer{Method: sc.Method, Value: sc.Value, Done: sc.Done}
		}
		sess.queueOut(&ServerComMessage{
			Meta: &MsgServerMeta{Id: id, Topic: t.original(asUid), Timestamp: &now, Cred: creds}})
		return nil
	}

	// Inform the requester that there are no credentials.
	sess.queueOut(NoContentParamsReply(msg, now, map[string]string{"what": "creds"}))

	return nil
}

// replySetCreds adds or validates user credentials such as email and phone numbers.
func (t *Topic) replySetCred(sess *Session, asUid types.Uid, authLevel auth.Level, msg *ClientComMessage) error {

	now := types.TimeNow()
	set := msg.Set
	incomingReqTs := msg.Timestamp

	if t.cat != types.TopicCatMe {
		sess.queueOut(ErrOperationNotAllowedReply(msg, now))
		return errors.New("invalid topic category for updating credentials")
	}

	var err error
	var tags []string
	creds := []MsgCredClient{*set.Cred}
	if set.Cred.Response != "" {
		// Credential is being validated. Return an arror if response is invalid.
		_, tags, err = validatedCreds(asUid, authLevel, creds, true)
	} else {
		// Credential is being added or updated.
		tmpToken, _, _ := store.GetLogicalAuthHandler("token").GenSecret(&auth.Rec{
			Uid:       asUid,
			AuthLevel: auth.LevelNone,
			Lifetime:  time.Hour * 24,
			Features:  auth.FeatureNoLogin})
		_, tags, err = addCreds(asUid, creds, nil, sess.lang, tmpToken)
	}

	if tags != nil {
		t.tags = tags
		t.presSubsOnline("tags", "", nilPresParams, nilPresFilters, "")
	}

	sess.queueOut(decodeStoreErrorExplicitTs(err, set.Id, t.original(asUid), now, incomingReqTs, nil))

	return err
}

// replyGetDel is a response to a get[what=del] request: load a list of deleted message ids, send them to
// a session as {meta}
// response goes to a single session rather than all sessions in a topic
func (t *Topic) replyGetDel(sess *Session, asUid types.Uid, req *MsgGetOpts, msg *ClientComMessage) error {
	now := types.TimeNow()
	toriginal := t.original(asUid)

	id := msg.Id
	incomingReqTs := msg.Timestamp

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}

	if req != nil && (req.IfModifiedSince != nil || req.User != "" || req.Topic != "") {
		sess.queueOut(ErrMalformedReply(msg, now))
		return errors.New("invalid MsgGetOpts query")
	}

	// Check if the user has permission to read the topic data and the request is valid.
	if userData := t.perUser[asUid]; asChan || (userData.modeGiven & userData.modeWant).IsReader() {
		ranges, delID, err := store.Messages.GetDeleted(t.name, asUid, msgOpts2storeOpts(req))
		if err != nil {
			sess.queueOut(ErrUnknownReply(msg, now))
			return err
		}

		if len(ranges) > 0 {
			sess.queueOut(&ServerComMessage{Meta: &MsgServerMeta{
				Id:    id,
				Topic: toriginal,
				Del: &MsgDelValues{
					DelId:  delID,
					DelSeq: delrangeDeserialize(ranges)},
				Timestamp: &now}})
			return nil
		}
	}

	sess.queueOut(NoContentParams(id, toriginal, now, incomingReqTs, map[string]string{"what": "del"}))

	return nil
}

// replyDelMsg deletes (soft or hard) messages in response to del.msg packet.
func (t *Topic) replyDelMsg(sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	now := types.TimeNow()
	del := msg.Del

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}
	if asChan {
		// Do not allow channel readers delete messages.
		sess.queueOut(ErrOperationNotAllowedReply(msg, now))
		return errors.New("channel readers cannot delete messages")
	}

	pud := t.perUser[asUid]
	if !(pud.modeGiven & pud.modeWant).IsDeleter() {
		// User must have an R permission: if the user cannot read messages, he has
		// no business of deleting them.
		if !(pud.modeGiven & pud.modeWant).IsReader() {
			sess.queueOut(ErrPermissionDeniedReply(msg, now))
			return errors.New("del.msg: permission denied")
		}

		// User has just the R permission, cannot hard-delete messages, silently
		// switching to soft-deleting
		del.Hard = false
	}

	var ranges []types.Range
	if len(del.DelSeq) == 0 {
		err = errors.New("del.msg: no IDs to delete")
	} else {
		count := 0
		for _, dq := range del.DelSeq {
			if dq.LowId > t.lastID || dq.LowId < 0 || dq.HiId < 0 ||
				(dq.HiId > 0 && dq.LowId > dq.HiId) ||
				(dq.LowId == 0 && dq.HiId == 0) {
				err = errors.New("del.msg: invalid entry in list")
				break
			}

			if dq.HiId > t.lastID {
				// Range is inclusive - exclusive [low, hi),
				// to delete all messages hi must be lastId + 1
				dq.HiId = t.lastID + 1
			} else if dq.LowId == dq.HiId || dq.LowId+1 == dq.HiId {
				dq.HiId = 0
			}

			if dq.HiId == 0 {
				count++
			} else {
				count += dq.HiId - dq.LowId
			}

			ranges = append(ranges, types.Range{Low: dq.LowId, Hi: dq.HiId})
		}

		if err == nil {
			// Sort by Low ascending then by Hi descending.
			sort.Sort(types.RangeSorter(ranges))
			// Collapse overlapping ranges
			ranges = types.RangeSorter(ranges).Normalize()
		}

		if count > defaultMaxDeleteCount && len(ranges) > 1 {
			err = errors.New("del.msg: too many messages to delete")
		}
	}

	if err != nil {
		sess.queueOut(ErrMalformedReply(msg, now))
		return err
	}

	forUser := asUid
	if del.Hard {
		forUser = types.ZeroUid
	}

	if err = store.Messages.DeleteList(t.name, t.delID+1, forUser, ranges); err != nil {
		sess.queueOut(ErrUnknownReply(msg, now))
		return err
	}

	// Increment Delete transaction ID
	t.delID++
	dr := delrangeDeserialize(ranges)
	if del.Hard {
		for uid, pud := range t.perUser {
			pud.delID = t.delID
			t.perUser[uid] = pud
		}
		// Broadcast the change to all, online and offline, exclude the session making the change.
		params := &presParams{delID: t.delID, delSeq: dr, actor: asUid.UserId()}
		filters := &presFilters{filterIn: types.ModeRead}
		t.presSubsOnline("del", params.actor, params, filters, sess.sid)
		t.presSubsOffline("del", params, filters, nilPresFilters, sess.sid, true)
	} else {
		pud := t.perUser[asUid]
		pud.delID = t.delID
		t.perUser[asUid] = pud

		// Notify user's other sessions
		t.presPubMessageDelete(asUid, pud.modeGiven&pud.modeWant, t.delID, dr, sess.sid)
	}

	sess.queueOut(NoErrParamsReply(msg, now, map[string]int{"del": t.delID}))

	return nil
}

// Shut down the topic in response to {del what="topic"} request
// See detailed description at hub.topicUnreg()
// 1. Checks if the requester is the owner. If so:
// 1.2 Evict all sessions
// 1.3 Ask hub to unregister self
// 1.4 Exit the run() loop
// 2. If requester is not the owner:
// 2.1 If this is a p2p topic:
// 2.1.1 Check if the other subscription still exists, if so, treat request as {leave unreg=true}
// 2.1.2 If the other subscription does not exist, delete topic
// 2.2 If this is not a p2p topic, treat it as {leave unreg=true}
func (t *Topic) replyDelTopic(h *Hub, sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	if t.owner != asUid {
		// Cases 2.1.1 and 2.2
		if t.cat != types.TopicCatP2P || t.subsCount() == 2 {
			return t.replyLeaveUnsub(h, sess, msg, asUid)
		}
	}

	// Notifications are sent from the topic loop.

	return nil
}

// Delete credential
func (t *Topic) replyDelCred(h *Hub, sess *Session, asUid types.Uid, authLvl auth.Level, msg *ClientComMessage) error {
	now := types.TimeNow()
	incomingReqTs := msg.Timestamp
	del := msg.Del

	if t.cat != types.TopicCatMe {
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return errors.New("del.cred: invalid topic category")
	}
	if del.Cred == nil || del.Cred.Method == "" {
		sess.queueOut(ErrMalformedReply(msg, now))
		return errors.New("del.cred: missing method")
	}

	tags, err := deleteCred(asUid, authLvl, del.Cred)
	if tags != nil {
		// Check if anything has been actually removed.
		_, removed := stringSliceDelta(t.tags, tags)
		if len(removed) > 0 {
			t.tags = tags
			t.presSubsOnline("tags", "", nilPresParams, nilPresFilters, "")
		}
	} else if err == nil {
		sess.queueOut(InfoNoActionReply(msg, now))
		return nil
	}
	sess.queueOut(decodeStoreErrorExplicitTs(err, del.Id, del.Topic, now, incomingReqTs, nil))
	return err
}

// Delete subscription.
func (t *Topic) replyDelSub(h *Hub, sess *Session, asUid types.Uid, msg *ClientComMessage) error {
	now := types.TimeNow()
	del := msg.Del

	asChan, err := t.verifyChannelAccess(msg.Original)
	if err != nil {
		// User should not be able to address non-channel topic as channel.
		sess.queueOut(ErrNotFoundReply(msg, now))
		return types.ErrNotFound
	}
	if asChan {
		// Don't allow channel readers to delete self-subscription. Use leave-unsub or del-topic.
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return errors.New("channel access denied: cannot delete subscription")
	}

	// Get ID of the affected user
	uid := types.ParseUserId(del.User)

	pud := t.perUser[asUid]
	if !(pud.modeGiven & pud.modeWant).IsAdmin() {
		err = errors.New("del.sub: permission denied")
	} else if uid.IsZero() || uid == asUid {
		// Cannot delete self-subscription. User [leave unsub] or [delete topic]
		err = errors.New("del.sub: cannot delete self-subscription")
	} else if t.cat == types.TopicCatP2P {
		// Don't try to delete the other P2P user
		err = errors.New("del.sub: cannot apply to a P2P topic")
	}

	if err != nil {
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return err
	}

	pud, ok := t.perUser[uid]
	if !ok {
		sess.queueOut(InfoNoActionReply(msg, now))
		return errors.New("del.sub: user not found")
	}

	// Check if the user being ejected is the owner.
	if (pud.modeGiven & pud.modeWant).IsOwner() {
		err = errors.New("del.sub: cannot evict topic owner")
	} else if !pud.modeWant.IsJoiner() {
		// If the user has banned the topic, subscription should not be deleted. Otherwise user may be re-invited
		// which defeats the purpose of banning.
		err = errors.New("del.sub: cannot delete banned subscription")
	}

	if err != nil {
		sess.queueOut(ErrPermissionDeniedReply(msg, now))
		return err
	}

	// Delete user's subscription from the database
	if err := store.Subs.Delete(t.name, uid); err != nil {
		if err == types.ErrNotFound {
			sess.queueOut(InfoNoActionReply(msg, now))
		} else {
			sess.queueOut(ErrUnknownReply(msg, now))
			return err
		}
	} else {
		sess.queueOut(NoErrReply(msg, now))
	}

	// Update cached unread count: negative value
	if (pud.modeWant & pud.modeGiven).IsReader() {
		usersUpdateUnread(uid, pud.readID-t.lastID, true)
	}

	// ModeUnset signifies deleted subscription as opposite to ModeNone - no access.
	t.notifySubChange(uid, asUid, false,
		pud.modeWant, pud.modeGiven, types.ModeUnset, types.ModeUnset, sess.sid)

	t.evictUser(uid, true, "")

	return nil
}

// replyLeaveUnsub is request to unsubscribe user and detach all user's sessions from topic.
// FIXME: if grp subscription does not exist, replyLeaveUnsub will replace grpXXX with chnXXX.
func (t *Topic) replyLeaveUnsub(h *Hub, sess *Session, msg *ClientComMessage, asUid types.Uid) error {
	now := types.TimeNow()

	if asUid.IsZero() {
		panic("replyLeaveUnsub: zero asUid")
	}

	if t.owner == asUid {
		if msg != nil {
			sess.queueOut(ErrPermissionDeniedReply(msg, now))
		}
		return errors.New("replyLeaveUnsub: owner cannot unsubscribe")
	}

	var err error
	var asChan bool
	if msg != nil {
		asChan, err = t.verifyChannelAccess(msg.Original)
		if err != nil {
			sess.queueOut(ErrNotFoundReply(msg, now))
			return errors.New("replyLeaveUnsub: incorrect addressing of channel")
		}
	}

	// Delete user's subscription from the database.
	if msg == nil && t.isChan {
		// Must try to unsubscribe both: as subscriber and as reader.
		err = store.Subs.Delete(t.name, asUid)
		if err == types.ErrNotFound {
			err = store.Subs.Delete(types.GrpToChn(t.name), asUid)
			asChan = true
		}
	} else if asChan {
		// Handle channel reader.
		err = store.Subs.Delete(types.GrpToChn(t.name), asUid)
	} else {
		// Handle subscriber.
		err = store.Subs.Delete(t.name, asUid)
	}

	if err != nil {
		if err == types.ErrNotFound {
			if msg != nil {
				sess.queueOut(InfoNoActionReply(msg, now))
			}
			err = nil
		} else if msg != nil {
			sess.queueOut(ErrUnknownReply(msg, now))
		}
		return err
	}

	if msg != nil {
		sess.queueOut(NoErrReply(msg, now))
	}

	var oldWant types.AccessMode
	var oldGiven types.AccessMode
	if !asChan {
		pud := t.perUser[asUid]

		// Update cached unread count: negative value
		if (pud.modeWant & pud.modeGiven).IsReader() {
			usersUpdateUnread(asUid, pud.readID-t.lastID, true)
		}
		oldWant, oldGiven = pud.modeWant, pud.modeGiven
	} else {
		oldWant, oldGiven = types.ModeCChnReader, types.ModeCChnReader
		// Unsubscribe user's devices from the channel (FCM topic).
		t.channelSubUnsub(asUid, false)
	}

	// Send prsence notifictions to admins, other users, and user's other sessions.
	t.notifySubChange(asUid, asUid, asChan, oldWant, oldGiven, types.ModeUnset, types.ModeUnset, sess.sid)

	// Evict all user's sessions, clear cached data, send notifications.
	t.evictUser(asUid, true, sess.sid)

	return nil
}

// evictUser evicts all given user's sessions from the topic and clears user's cached data, if appropriate.
func (t *Topic) evictUser(uid types.Uid, unsub bool, skip string) {
	now := types.TimeNow()
	pud, ok := t.perUser[uid]

	// Detach user from topic
	if unsub {
		if t.cat == types.TopicCatP2P {
			// P2P: mark user as deleted
			pud.online = 0
			pud.deleted = true
			t.perUser[uid] = pud
		} else if ok {
			// Grp: delete per-user data
			delete(t.perUser, uid)
			t.computePerUserAcsUnion()

			usersRegisterUser(uid, false)
		}
	} else if ok {
		// Clear online status
		pud.online = 0
		t.perUser[uid] = pud
	}

	// Detach all user's sessions
	msg := NoErrEvicted("", t.original(uid), now)
	msg.Ctrl.Params = map[string]interface{}{"unsub": unsub}
	msg.SkipSid = skip
	msg.uid = uid
	msg.AsUser = uid.UserId()
	for s := range t.sessions {
		if pssd, removed := t.remSession(s, uid); pssd != nil {
			if removed {
				s.detachSession(t.name)
			}
			if s.sid != skip {
				s.queueOut(msg)
			}
		}
	}
}

// User's subscription to a topic has changed, send presence notifications.
// 1. New subscription
// 2. Deleted subscription
// 3. Permissions changed
// Sending to
// (a) Topic admins online on topic itself.
// (b) Topic admins offline on 'me' if approval is needed.
// (c) If subscription is deleted, 'gone' to target.
// (d) 'off' to topic members online if deleted or muted.
// (e) To target user.
func (t *Topic) notifySubChange(uid, actor types.Uid, isChan bool,
	oldWant, oldGiven, newWant, newGiven types.AccessMode, skip string) {

	unsub := newWant == types.ModeUnset || newGiven == types.ModeUnset

	target := uid.UserId()

	dWant := types.ModeNone.String()
	if newWant.IsDefined() {
		if oldWant.IsDefined() && !oldWant.IsZero() {
			dWant = oldWant.Delta(newWant)
		} else {
			dWant = newWant.String()
		}
	}

	dGiven := types.ModeNone.String()
	if newGiven.IsDefined() {
		if oldGiven.IsDefined() && !oldGiven.IsZero() {
			dGiven = oldGiven.Delta(newGiven)
		} else {
			dGiven = newGiven.String()
		}
	}
	params := &presParams{
		target: target,
		actor:  actor.UserId(),
		dWant:  dWant,
		dGiven: dGiven}

	filter := &presFilters{
		filterIn:    types.ModeCSharer,
		excludeUser: target}

	// Announce the change in permissions to the admins who are online in the topic, exclude the target
	// and exclude the actor's session.
	t.presSubsOnline("acs", target, params, filter, skip)

	// If it's a new subscription or if the user asked for permissions in excess of what was granted,
	// announce the request to topic admins on 'me' so they can approve the request. The notification
	// is not sent to the target user or the actor's session.
	if newWant.BetterThan(newGiven) || oldWant == types.ModeNone {
		t.presSubsOffline("acs", params, filter, filter, skip, true)
	}

	// Handling of muting/unmuting.
	// Case A: subscription deleted.
	// Case B: subscription muted only.
	if unsub {
		// Subscription deleted
		// In case of a P2P topic subscribe/unsubscribe users from each other's notifications.
		if t.cat == types.TopicCatP2P {
			uid2 := t.p2pOtherUser(uid)
			// Remove user1's subscription to user2 and notify user1's other sessions that he is gone.
			t.presSingleUserOffline(uid, newWant&newGiven, "gone", nilPresParams, skip, false)
			// Tell user2 that user1 is offline but let him keep sending updates in case user1 resubscribes.
			presSingleUserOfflineOffline(uid2, target, "off", nilPresParams, "")
		} else if t.cat == types.TopicCatGrp && !isChan {
			// Notify all sharers that the user is offline now.
			t.presSubsOnline("off", uid.UserId(), nilPresParams, filter, skip)
		}
	} else if !(newWant & newGiven).IsPresencer() && (oldWant & oldGiven).IsPresencer() {
		// Subscription just muted.
		var source string
		if t.cat == types.TopicCatP2P {
			source = t.p2pOtherUser(uid).UserId()
		} else if t.cat == types.TopicCatGrp && !isChan {
			source = t.name
		}
		if source != "" {
			// Tell user1 to start discarding updates from muted topic/user.
			presSingleUserOfflineOffline(uid, source, "off+dis", nilPresParams, "")
		}

	} else if (newWant & newGiven).IsPresencer() && !(oldWant & oldGiven).IsPresencer() {
		// Subscription un-muted.

		// Notify subscriber of topic's online status.
		if t.cat == types.TopicCatGrp && !isChan {
			t.presSingleUserOffline(uid, newWant&newGiven, "?unkn+en", nilPresParams, "", false)
		} else if t.cat == types.TopicCatMe {
			// User is visible online now, notify subscribers.
			t.presUsersOfInterest("on+en", t.userAgent)
		}
	}

	// Notify target that permissions have changed.
	if !unsub {
		// Notify sessions online in the topic.
		t.presSubsOnlineDirect("acs", params, &presFilters{singleUser: target}, skip)
		// Notify target's other sessions on 'me'.
		t.presSingleUserOffline(uid, newWant&newGiven, "acs", params, skip, true)
	}
}

// Prepares a payload to be delivered to a mobile device as a push notification in response to a {data} message.
func (t *Topic) pushForData(fromUid types.Uid, data *MsgServerData) *push.Receipt {
	// The `Topic` in the push receipt is `t.xoriginal` for group topics, `fromUid` for p2p topics,
	// not the t.original(fromUid) because it's the topic name as seen by the recipient, not by the sender.
	topic := t.xoriginal
	if t.cat == types.TopicCatP2P {
		topic = fromUid.UserId()
	}

	// Initialize the push receipt.
	contentType, _ := data.Head["mime"].(string)
	receipt := push.Receipt{
		To: make(map[types.Uid]push.Recipient, t.subsCount()),
		Payload: push.Payload{
			What:        push.ActMsg,
			Silent:      false,
			Topic:       topic,
			From:        data.From,
			Timestamp:   data.Timestamp,
			SeqId:       data.SeqId,
			ContentType: contentType,
			Content:     data.Content}}

	if t.isChan {
		receipt.Channel = types.GrpToChn(t.xoriginal)
	}

	for uid, pud := range t.perUser {
		// Send only to those who have notifications enabled, exclude the originating user.
		if uid == fromUid {
			continue
		}
		mode := pud.modeWant & pud.modeGiven
		if mode.IsPresencer() && mode.IsReader() && !pud.deleted {
			receipt.To[uid] = push.Recipient{
				// Number of sessions this data message will be delivered to.
				// Push notifications sent to users with non-zero online sessions will be marked silent.
				Delivered: pud.online,
			}
		}
	}
	if len(receipt.To) > 0 || receipt.Channel != "" {
		return &receipt
	}
	// If there are no recipient there is no need to send the push notification.
	return nil
}

// Prepares payload to be delivered to a mobile device as a push notification in response to a new subscription.
func (t *Topic) pushForSub(fromUid, toUid types.Uid, want, given types.AccessMode, now time.Time) *push.Receipt {
	// The `Topic` in the push receipt is `t.xoriginal` for group topics, `fromUid` for p2p topics,
	// not the t.original(fromUid) because it's the topic name as seen by the recipient, not by the sender.
	topic := t.xoriginal
	if t.cat == types.TopicCatP2P {
		topic = fromUid.UserId()
	}

	// Initialize the push receipt.
	receipt := push.Receipt{
		To: make(map[types.Uid]push.Recipient, t.subsCount()),
		Payload: push.Payload{
			What:      push.ActSub,
			Silent:    false,
			Topic:     topic,
			From:      fromUid.UserId(),
			Timestamp: now,
			SeqId:     t.lastID,
			ModeWant:  want,
			ModeGiven: given}}

	receipt.To[toUid] = push.Recipient{}

	return &receipt
}

// FIXME: this won't work correctly with multiplexing sessions.
func (t *Topic) mostRecentSession() *Session {
	var sess *Session
	var latest time.Time
	for s := range t.sessions {
		if s.lastAction.After(latest) {
			sess = s
			latest = s.lastAction
		}
	}
	return sess
}

const (
	// Topic is fully initialized.
	topicStatusLoaded = 0x1
	// Topic is paused: all packets are rejected.
	topicStatusPaused = 0x2

	// Topic is in the process of being deleted. This is irrecoverable.
	topicStatusMarkedDeleted = 0x10
	// Topic is suspended: read-only mode.
	topicStatusReadOnly = 0x20
)

// statusChangeBits sets or removes given bits from t.status
func (t *Topic) statusChangeBits(bits int32, set bool) {
	for {
		oldStatus := atomic.LoadInt32((*int32)(&t.status))
		newStatus := oldStatus
		if set {
			newStatus = newStatus | bits
		} else {
			newStatus = newStatus & ^bits
		}
		if newStatus == oldStatus {
			break
		}
		if atomic.CompareAndSwapInt32((*int32)(&t.status), oldStatus, newStatus) {
			break
		}
	}
}

// markLoaded indicates that topic subscribers have been loaded into memory.
func (t *Topic) markLoaded() {
	t.statusChangeBits(topicStatusLoaded, true)
}

// markPaused pauses or unpauses the topic. When the topic is paused all
// messages are rejected.
func (t *Topic) markPaused(pause bool) {
	t.statusChangeBits(topicStatusPaused, pause)
}

// markDeleted marks topic as being deleted.
func (t *Topic) markDeleted() {
	t.statusChangeBits(topicStatusMarkedDeleted, true)
}

// markReadOnly suspends/un-suspends the topic: adds or removes the 'read-only' flag.
func (t *Topic) markReadOnly(readOnly bool) {
	t.statusChangeBits(topicStatusReadOnly, readOnly)
}

// isInactive checks if topic is paused or being deleted.
func (t *Topic) isInactive() bool {
	return (atomic.LoadInt32((*int32)(&t.status)) & (topicStatusPaused | topicStatusMarkedDeleted)) != 0
}

func (t *Topic) isReadOnly() bool {
	return (atomic.LoadInt32((*int32)(&t.status)) & topicStatusReadOnly) != 0
}

func (t *Topic) isLoaded() bool {
	return (atomic.LoadInt32((*int32)(&t.status)) & topicStatusLoaded) != 0
}

func (t *Topic) isDeleted() bool {
	return (atomic.LoadInt32((*int32)(&t.status)) & topicStatusMarkedDeleted) != 0
}

// Get topic name suitable for the given client
func (t *Topic) original(uid types.Uid) string {
	if t.cat == types.TopicCatP2P {
		if pud, ok := t.perUser[uid]; ok {
			return pud.topicName
		}
		panic("Invalid P2P topic")
	}

	if t.cat == types.TopicCatGrp && t.isChan {
		if _, ok := t.perUser[uid]; !ok {
			// This is a channel reader.
			return types.GrpToChn(t.xoriginal)
		}
	}
	return t.xoriginal
}

// Get ID of the other user in a P2P topic
func (t *Topic) p2pOtherUser(uid types.Uid) types.Uid {
	if t.cat == types.TopicCatP2P {
		// Try to find user in subscribers.
		for u2 := range t.perUser {
			if u2.Compare(uid) != 0 {
				return u2
			}
		}
	}

	// Even when one user is deleted, the subscription must be restored
	// before p2pOtherUser is called.
	panic("Not a valid P2P topic")
}

// Get per-session value of fnd.Public
func (t *Topic) fndGetPublic(sess *Session) interface{} {
	if t.cat == types.TopicCatFnd {
		if t.public == nil {
			return nil
		}
		if pubmap, ok := t.public.(map[string]interface{}); ok {
			return pubmap[sess.sid]
		}
		panic("Invalid Fnd.Public type")
	}
	panic("Not Fnd topic")
}

// Assign per-session fnd.Public. Returns true if value has been changed.
func (t *Topic) fndSetPublic(sess *Session, public interface{}) bool {
	if t.cat != types.TopicCatFnd {
		panic("Not Fnd topic")
	}

	var pubmap map[string]interface{}
	var ok bool
	if t.public != nil {
		if pubmap, ok = t.public.(map[string]interface{}); !ok {
			// This could only happen if fnd.public is assigned outside of this function.
			panic("Invalid Fnd.Public type")
		}
	}
	if pubmap == nil {
		pubmap = make(map[string]interface{})
	}

	if public != nil {
		pubmap[sess.sid] = public
	} else {
		ok = (pubmap[sess.sid] != nil)
		delete(pubmap, sess.sid)
		if len(pubmap) == 0 {
			pubmap = nil
		}
	}
	t.public = pubmap
	return ok

}

// Remove per-session value of fnd.Public.
func (t *Topic) fndRemovePublic(sess *Session) {
	if t.public == nil {
		return
	}
	// FIXME: case of a multiplexing session won't work correctly.
	// Maybe handle it at the proxy topic.
	if pubmap, ok := t.public.(map[string]interface{}); ok {
		delete(pubmap, sess.sid)
		return
	}
	panic("Invalid Fnd.Public type")
}

func (t *Topic) accessFor(authLvl auth.Level) types.AccessMode {
	return selectAccessMode(authLvl, t.accessAnon, t.accessAuth, getDefaultAccess(t.cat, true, false))
}

// subsCount returns the number of topic subsribers
func (t *Topic) subsCount() int {
	if t.cat == types.TopicCatP2P {
		count := 0
		for uid := range t.perUser {
			if !t.perUser[uid].deleted {
				count++
			}
		}
		return count
	}
	return len(t.perUser)
}

// Adds a new multiplex proxied session to the topic's clusterWriteLoop.
func (t *Topic) addProxiedSession(s *Session) {
	t.proxiedSessions = append(t.proxiedSessions, s)
	if len(t.proxiedSessions) == 1 {
		t.proxiedChannels = make([]reflect.SelectCase, 1+3)
		continueChan := make(chan struct{})
		t.proxiedChannels[0] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(continueChan)}
		t.proxiedChannels[1] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.send)}
		t.proxiedChannels[2] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.stop)}
		t.proxiedChannels[3] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.detach)}
		go t.clusterWriteLoop()
	} else {
		t.proxiedChannels = append(t.proxiedChannels, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.send)})
		t.proxiedChannels = append(t.proxiedChannels, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.stop)})
		t.proxiedChannels = append(t.proxiedChannels, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.detach)})
		// Send an interrupt signal to clusterWriteLoop. New session added.
		t.proxiedChannels[0].Chan.Interface().(chan struct{}) <- struct{}{}
	}
}

// Removes a multiplex proxied session from the topic's clusterWriteLoop.
func (t *Topic) remProxiedSession(sess *Session) bool {
	for i, s := range t.proxiedSessions {
		if sess == s {
			interruptChan := t.proxiedChannels[0].Chan.Interface().(chan struct{})
			if len(t.proxiedSessions) == 1 {
				t.proxiedSessions = nil
				t.proxiedChannels = nil
			} else {
				n := len(t.proxiedSessions)
				// Move last session into position i.
				t.proxiedSessions[i] = t.proxiedSessions[n-1]
				t.proxiedSessions[n-1] = nil
				t.proxiedSessions = t.proxiedSessions[:n-1]

				// Move channels into position i.
				for j := 0; j < 3; j++ {
					to := i*3 + 1 + j
					from := (n-1)*3 + 1 + j
					t.proxiedChannels[to] = t.proxiedChannels[from]
				}
				numChans := len(t.proxiedChannels) - 3
				t.proxiedChannels = t.proxiedChannels[:numChans]
			}
			interruptChan <- struct{}{}
			return true
		}
	}
	return false
}

// Add session record. 'user' may be different from sess.uid.
func (t *Topic) addSession(sess *Session, asUid types.Uid, isChanSub bool) bool {
	s := sess
	if sess.multi != nil {
		s = s.multi
	}

	if pssd, ok := t.sessions[s]; ok {
		// Subscription already exists.
		if s.isMultiplex() && !sess.background {
			// This slice is expected to be relatively short.
			// Not doing anything fancy here like maps or sorting.
			pssd.muids = append(pssd.muids, asUid)
			t.sessions[s] = pssd
		}

		// Maybe panic here.
		return false
	}

	if s.isMultiplex() {
		if sess.background {
			t.sessions[s] = perSessionData{}
		} else {
			t.sessions[s] = perSessionData{muids: []types.Uid{asUid}}
		}
		t.addProxiedSession(s)
	} else {
		t.sessions[s] = perSessionData{uid: asUid, isChanSub: isChanSub}
	}

	return true
}

// Disconnects session from topic if either one of the following is true:
// * 's' is an ordinary session AND ('asUid' is zero OR 'asUid' matches subscribed user).
// * 's' is a multiplexing session and it's being dropped all together ('asUid' is zero ).
// If 's' is a multiplexing session and asUid is not zero, it's removed from the list of session
// users 'muids'.
// Returns perSessionData if it was found and true if session was actually detached from topic.
func (t *Topic) remSession(sess *Session, asUid types.Uid) (*perSessionData, bool) {
	s := sess
	if sess.multi != nil {
		s = s.multi
	}
	pssd, ok := t.sessions[s]
	if !ok {
		// Session not found at all.
		return nil, false
	}

	if pssd.uid == asUid || asUid.IsZero() {
		delete(t.sessions, s)
		if s.isMultiplex() && !t.remProxiedSession(s) {
			log.Printf("topic[%s]: multiplex session %s not removed from the event loop", t.name, s.sid)
		}
		return &pssd, true
	}

	for i := range pssd.muids {
		if pssd.muids[i] == asUid {
			pssd.muids[i] = pssd.muids[len(pssd.muids)-1]
			pssd.muids = pssd.muids[:len(pssd.muids)-1]
			t.sessions[s] = pssd
			if len(pssd.muids) == 0 {
				delete(t.sessions, s)
				return &pssd, true
			} else {
				return &pssd, false
			}
		}
	}

	return nil, false
}

// Check if topic has any online (non-background) users.
func (t *Topic) isOnline() bool {
	// Find at least one non-background session.
	for s, pssd := range t.sessions {
		if s.isMultiplex() && len(pssd.muids) > 0 {
			return true
		}
		if !s.background {
			return true
		}
	}
	return false
}

// Verifies if topic can be access by the given name: access any topic as non-channel, access channel as channel.
// Returns true if access is for channel, false if not and error if access is invalid.
func (t *Topic) verifyChannelAccess(asTopic string) (bool, error) {
	if !isChannel(asTopic) {
		return false, nil
	}
	if t.isChan {
		return true, nil
	}
	return false, types.ErrNotFound
}

// Infer topic category from name.
func topicCat(name string) types.TopicCat {
	return types.GetTopicCat(name)
}

// Generate random string as a name of the group topic
func genTopicName() string {
	return "grp" + store.GetUidString()
}

// Check if group topic is referenced as a channel.
// The "nch" should not be considered a channel reference because it can only be used by the topic owner at the time of
// group topic creation.
func isChannel(name string) bool {
	return strings.HasPrefix(name, "chn")
}

// Convert expanded (routable) topic name into name suitable for sending to the user.
// For example p2pAbCDef123 -> usrAbCDef
func topicNameForUser(name string, uid types.Uid, isChan bool) string {
	switch topicCat(name) {
	case types.TopicCatMe:
		return "me"
	case types.TopicCatFnd:
		return "fnd"
	case types.TopicCatP2P:
		uid1, uid2, _ := types.ParseP2P(name)
		if uid == uid1 {
			return uid2.UserId()
		}
		return uid1.UserId()
	case types.TopicCatGrp:
		if isChan {
			return types.GrpToChn(name)
		}
	}
	return name
}
