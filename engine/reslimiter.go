/*
Real-time Charging System for Telecom & ISP environments
Copyright (C) ITsysCOM GmbH

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package engine

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cgrates/cgrates/cache2go"
	"github.com/cgrates/cgrates/config"
	"github.com/cgrates/cgrates/utils"
	"github.com/cgrates/rpcclient"
)

// ResourceLimit represents a limit imposed for accessing a resource (eg: new calls)
type ResourceLimit struct {
	ID             string           // Identifier of this limit
	Filters        []*RequestFilter // Filters for the request
	ActivationTime time.Time        // Time when this limit becomes active
	Weight         float64          // Weight to sort the ResourceLimits
	Limit          float64          // Limit value
	ActionTriggers ActionTriggers   // Thresholds to check after changing Limit
	Used           utils.Int64Slice // []time.Time.Unix() - keep it in this format so we can expire usage automatically
}

// Pas the config as a whole so we can ask access concurrently
func NewResourceLimiterService(cfg *config.CGRConfig, dataDB AccountingStorage, cdrStatS rpcclient.RpcClientConnection) (*ResourceLimiterService, error) {
	rls := &ResourceLimiterService{stringIndexes: make(map[string]map[string]utils.StringMap), dataDB: dataDB, cdrStatS: cdrStatS}
	return rls, nil
}

// ResourcesLimiter is the service handling channel limits
type ResourceLimiterService struct {
	sync.RWMutex
	stringIndexes map[string]map[string]utils.StringMap // map[fieldName]map[fieldValue]utils.StringMap[resourceID]
	dataDB        AccountingStorage                     // So we can load the data in cache and index it
	cdrStatS      rpcclient.RpcClientConnection
}

// Index cached ResourceLimits with MetaString filter types
func (rls *ResourceLimiterService) indexStringFilters(rlIDs []string) error {
	utils.Logger.Info("<RLs> Start indexing string filters")
	newStringIndexes := make(map[string]map[string]utils.StringMap) // Index it transactionally
	var cacheIDsToIndex []string                                    // Cache keys of RLs to be indexed
	if rlIDs == nil {
		cacheIDsToIndex = cache2go.GetEntriesKeys(utils.ResourceLimitsPrefix)
	} else {
		for _, rlID := range rlIDs {
			cacheIDsToIndex = append(cacheIDsToIndex, utils.ResourceLimitsPrefix+rlID)
		}
	}
	for _, cacheKey := range cacheIDsToIndex {
		x, ok := cache2go.Get(cacheKey)
		if !ok {
			return utils.ErrNotFound
		}
		rl := x.(*ResourceLimit)
		for _, fltr := range rl.Filters {
			if fltr.Type != MetaString {
				continue
			}
			if _, hastIt := newStringIndexes[fltr.FieldName]; !hastIt {
				newStringIndexes[fltr.FieldName] = make(map[string]utils.StringMap)
			}
			for _, fldVal := range fltr.Values {
				if _, hasIt := newStringIndexes[fltr.FieldName][fldVal]; !hasIt {
					newStringIndexes[fltr.FieldName][fldVal] = make(utils.StringMap)
				}
				newStringIndexes[fltr.FieldName][fldVal][rl.ID] = true
			}
		}
	}
	rls.Lock()
	defer rls.Unlock()
	if rlIDs == nil { // We have rebuilt complete index
		rls.stringIndexes = newStringIndexes
		return nil
	}
	// Merge the indexes since we have only performed limited indexing
	for fldNameKey, mpFldName := range newStringIndexes {
		if _, hasIt := rls.stringIndexes[fldNameKey]; !hasIt {
			rls.stringIndexes[fldNameKey] = mpFldName
		} else {
			for fldValKey, strMap := range newStringIndexes[fldNameKey] {
				if _, hasIt := rls.stringIndexes[fldNameKey][fldValKey]; !hasIt {
					rls.stringIndexes[fldNameKey][fldValKey] = strMap
				} else {
					for resIDKey := range newStringIndexes[fldNameKey][fldValKey] {
						rls.stringIndexes[fldNameKey][fldValKey][resIDKey] = true
					}
				}
			}
		}
	}
	utils.Logger.Info("<RLs> Done indexing string filters")
	return nil
}

// Called when cache/re-caching is necessary
func (rls *ResourceLimiterService) cacheResourceLimits(loadID string, rlIDs []string) error {
	if len(rlIDs) == 0 {
		return nil
	}
	if rlIDs == nil {
		utils.Logger.Info("<RLs> Start caching all resource limits")
	} else if len(rlIDs) != 0 {
		utils.Logger.Info(fmt.Sprintf("<RLs> Start caching resource limits with ids: %+v", rlIDs))
	}
	if err := rls.dataDB.PreloadCacheForPrefix(utils.ResourceLimitsPrefix); err != nil {
		return err
	}
	utils.Logger.Info("<RLs> Done caching resource limits")
	return rls.indexStringFilters(rlIDs)
}

// Called to start the service
func (rls *ResourceLimiterService) Start() error {
	if err := rls.cacheResourceLimits("ResourceLimiterServiceStart", nil); err != nil {
		return err
	}
	return nil
}

// Called to shutdown the service
func (rls *ResourceLimiterService) Shutdown() error {
	return nil
}

// RPC Methods available internally

// Cache/Re-cache
func (rls *ResourceLimiterService) V1CacheResourceLimits(attrs *utils.AttrRLsCache, reply *string) error {
	if err := rls.cacheResourceLimits(attrs.LoadID, attrs.ResourceLimitIDs); err != nil {
		return err
	}
	*reply = utils.OK
	return nil
}

// Make the service available as RPC internally
func (rls *ResourceLimiterService) Call(serviceMethod string, args interface{}, reply interface{}) error {
	parts := strings.Split(serviceMethod, ".")
	if len(parts) != 2 {
		return utils.ErrNotImplemented
	}
	// get method
	method := reflect.ValueOf(rls).MethodByName(parts[0][len(parts[0])-2:] + parts[1]) // Inherit the version in the method
	if !method.IsValid() {
		return utils.ErrNotImplemented
	}

	// construct the params
	params := []reflect.Value{reflect.ValueOf(args), reflect.ValueOf(reply)}
	ret := method.Call(params)
	if len(ret) != 1 {
		return utils.ErrServerError
	}
	if ret[0].Interface() == nil {
		return nil
	}
	err, ok := ret[0].Interface().(error)
	if !ok {
		return utils.ErrServerError
	}
	return err
}