package service

import (
	"github.com/v2rayA/v2rayA/db"
	"github.com/v2rayA/v2rayA/db/configure"
)

// A subscription's database ID stays stable when another subscription is
// inserted or removed while the periodic update is in progress.
func UpdateSubscriptionByID(id int64) error {
	index, err := db.GetSubscriptionIndexByID(id)
	if err != nil {
		return err
	}
	return UpdateSubscription(index, false)
}

func AutoSelectServersFromSubscriptionIDs(ids []int64, disconnect bool) error {
	for _, id := range ids {
		index, err := db.GetSubscriptionIndexByID(id)
		if err != nil {
			return err
		}
		subscription := configure.GetSubscription(index)
		if subscription != nil && subscription.AutoSelect {
			if err := SelectServersFromSubscription(index, disconnect); err != nil {
				return err
			}
		}
	}
	return nil
}
