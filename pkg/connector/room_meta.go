package connector

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/Leicas/matrisms/pkg/common"
)

var _ bridgev2.RoomNameHandlingNetworkAPI = (*SMSClient)(nil)

// HandleMatrixRoomName syncs a room rename done in the Matrix client into the
// VoIP.ms phonebook — the Element-native equivalent of `!matrisms rename`.
// bridgev2 only calls this when the new name differs from the portal's
// current one, so bridge-initiated renames don't loop back here.
func (c *SMSClient) HandleMatrixRoomName(ctx context.Context, msg *bridgev2.MatrixRoomName) (bool, error) {
	_, peer, ok := common.ParsePortalID(msg.Portal.ID)
	if !ok || peer == "" {
		return false, fmt.Errorf("only conversation rooms can be renamed")
	}
	name := strings.TrimSpace(msg.Content.Name)
	if name == "" {
		return false, fmt.Errorf("room name is empty")
	}

	if _, err := c.API.SetContactName(ctx, peer, name); err != nil {
		return false, fmt.Errorf("saving to the VoIP.ms phonebook: %w", err)
	}
	for _, client := range c.Main.activeClients() {
		client.InvalidatePhonebook()
	}

	// The room itself already carries the new name; refresh the peer ghost so
	// the sender profile follows the contact.
	if ghost, err := c.UserLogin.Bridge.GetGhostByID(ctx, common.PhoneToGhostID(peer)); err == nil {
		if userInfo, err := c.Main.GetUserInfo(ctx, ghost); err == nil {
			ghost.UpdateInfo(ctx, userInfo)
		}
	}

	msg.Portal.Name = name
	msg.Portal.NameSet = true
	return true, nil
}
