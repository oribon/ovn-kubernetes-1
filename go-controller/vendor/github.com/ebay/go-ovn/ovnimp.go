/**
 * Copyright (c) 2017 eBay Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 **/

package goovn

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ebay/libovsdb"

	"k8s.io/klog/v2"
)

const (
	commitTransactionText = "committing transaction"
)

var (
	// ErrorOption used when invalid args specified
	ErrorOption = errors.New("invalid option specified")
	// ErrorSchema used when something wrong in ovnnb
	ErrorSchema = errors.New("table schema error")
	// ErrorNotFound used when object not found in ovnnb
	ErrorNotFound = errors.New("object not found")
	// ErrorExist used when object already exists in ovnnb
	ErrorExist = errors.New("object exist")
	// ErrorNoChanges used when function called, but no changes
	ErrorNoChanges = errors.New("no changes requested")
	// ErrorDuplicateName used when multiple rows are found when searching by name
	ErrorDuplicateName = errors.New("duplicate name")
)

// OVNRow ovn nb/sb row
type OVNRow map[string]interface{}

func (odbi *ovndb) getRowUUIDs(table string, row OVNRow) []string {
	var uuids []string
	var wildcard bool

	if reflect.DeepEqual(row, make(OVNRow)) {
		wildcard = true
	}

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheTable, ok := odbi.cache[table]
	if !ok {
		return nil
	}

	for uuid, drows := range cacheTable {
		if wildcard {
			uuids = append(uuids, uuid)
			continue
		}

		isEqual := true
		for field, value := range row {
			if v, ok := drows.Fields[field]; ok {
				if v != value {
					isEqual = false
					break
				}
			}
		}
		if isEqual {
			uuids = append(uuids, uuid)
		}
	}

	return uuids
}

func (odbi *ovndb) getRowUUID(table string, row OVNRow) string {
	uuids := odbi.getRowUUIDs(table, row)
	if len(uuids) > 0 {
		return uuids[0]
	}
	return ""
}

//test if map s contains t
//This function is not both s and t are nil at same time
func (odbi *ovndb) oMapContians(s, t map[interface{}]interface{}) bool {
	if s == nil || t == nil {
		return false
	}

	for tk, tv := range t {
		if sv, ok := s[tk]; !ok {
			return false
		} else if tv != sv {
			return false
		}
	}
	return true
}

func (odbi *ovndb) getRowUUIDContainsUUID(table, field, uuid string) (string, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheTable, ok := odbi.cache[table]
	if !ok {
		return "", ErrorSchema
	}

	for id, drows := range cacheTable {
		v := fmt.Sprintf("%s", drows.Fields[field])
		if strings.Contains(v, uuid) {
			return id, nil
		}
	}
	return "", ErrorNotFound
}

func (odbi *ovndb) getRowsMatchingUUID(table, field, uuid string) ([]string, error) {
	odbi.cachemutex.Lock()
	defer odbi.cachemutex.Unlock()
	var uuids []string
	for id, drows := range odbi.cache[table] {
		v := fmt.Sprintf("%s", drows.Fields[field])
		if strings.Contains(v, uuid) {
			uuids = append(uuids, id)
		}
	}
	if len(uuids) == 0 {
		return uuids, ErrorNotFound
	}
	return uuids, nil
}

func (odbi *ovndb) transact(db string, ops ...libovsdb.Operation) ([]libovsdb.OperationResult, error) {
	odbi.tranmutex.RLock()
	defer odbi.tranmutex.RUnlock()
	client, err := odbi.getClient()
	if err != nil {
		return nil, err
	}

	reply, err := client.Transact(db, ops...)
	if err != nil {
		return reply, err
	}

	// Per RFC 7047 Section 4.1.3, the operation result array in the transact response object
	// maps one-to-one with operations array in the transact request object. We need to check
	// each of the operation result for null error to ensure that the transaction has succeeded.
	for i, o := range reply {
		if o.Error != "" {
			// Per RFC 7047 Section 4.1.3, if all of the operations succeed, but the results
			// cannot be committed, then "result" will have one more element than "params",
			// with the additional element being an <error>.
			opsInfo := commitTransactionText
			if i < len(ops) {
				opsInfo = fmt.Sprintf("%v", ops[i])
			}
			odbi.close()
			return nil, fmt.Errorf("Reconnecting...Transaction Failed due to an error: %v details: %v in %s",
				o.Error, o.Details, opsInfo)
		}
	}
	if len(reply) < len(ops) {
		return reply, fmt.Errorf("Number of Replies should be atleast equal to number of operations")
	}
	return reply, nil
}

func (odbi *ovndb) execute(cmds ...*OvnCommand) error {
	_, err := odbi.ExecuteR(cmds...)
	return err
}

func (odbi *ovndb) executeR(cmds ...*OvnCommand) ([]string, error) {
	if cmds == nil {
		return nil, nil
	}
	var ops []libovsdb.Operation
	for _, cmd := range cmds {
		if cmd != nil {
			ops = append(ops, cmd.Operations...)
		}
	}

	results, err := odbi.transact(odbi.db, ops...)
	if err != nil {
		return nil, err
	}

	// The total number of UUIDs will be <= number of results returned.
	UUIDs := make([]string, 0, len(results))
	for _, r := range results {
		if len(r.UUID.GoUUID) > 0 {
			UUIDs = append(UUIDs, r.UUID.GoUUID)
		}
	}

	if len(UUIDs) > 0 {
		return UUIDs, nil
	}

	return nil, nil
}

func (odbi *ovndb) float64_to_int(row libovsdb.Row) {
	for field, value := range row.Fields {
		if v, ok := value.(float64); ok {
			n := int(v)
			if float64(n) == v {
				row.Fields[field] = n
			}
		}
	}
}

func (odbi *ovndb) signalCreate(table, uuid string) {
	switch table {
	case TableLogicalRouter:
		lr := odbi.rowToLogicalRouter(uuid)
		odbi.signalCB.OnLogicalRouterCreate(lr)
	case TableLogicalRouterPort:
		lrp := odbi.rowToLogicalRouterPort(uuid)
		odbi.signalCB.OnLogicalRouterPortCreate(lrp)
	case TableLogicalRouterStaticRoute:
		lrsr := odbi.rowToLogicalRouterStaticRoute(uuid)
		odbi.signalCB.OnLogicalRouterStaticRouteCreate(lrsr)
	case TableLogicalSwitch:
		ls := odbi.rowToLogicalSwitch(uuid)
		odbi.signalCB.OnLogicalSwitchCreate(ls)
	case TableLogicalSwitchPort:
		lp, err := odbi.uuidToLogicalPort(uuid)
		if err == nil {
			odbi.signalCB.OnLogicalPortCreate(lp)
		}
	case TableACL:
		acl := odbi.rowToACL(uuid)
		odbi.signalCB.OnACLCreate(acl)
	case TableDHCPOptions:
		dhcp := odbi.rowToDHCPOptions(uuid)
		odbi.signalCB.OnDHCPOptionsCreate(dhcp)
	case TableQoS:
		qos := odbi.rowToQoS(uuid)
		odbi.signalCB.OnQoSCreate(qos)
	case TableLoadBalancer:
		lb, _ := odbi.rowToLB(uuid)
		odbi.signalCB.OnLoadBalancerCreate(lb)
	case TableMeter:
		meter := odbi.rowToMeter(uuid)
		odbi.signalCB.OnMeterCreate(meter)
	case TableMeterBand:
		band, _ := odbi.rowToMeterBand(uuid)
		odbi.signalCB.OnMeterBandCreate(band)
	case TableChassis:
		chassis, _ := odbi.rowToChassis(uuid)
		odbi.signalCB.OnChassisCreate(chassis)
	case TableEncap:
		encap, _ := odbi.rowToEncap(uuid)
		odbi.signalCB.OnEncapCreate(encap)
	}
}

func (odbi *ovndb) signalDelete(table, uuid string) {
	switch table {
	case TableLogicalRouter:
		lr := odbi.rowToLogicalRouter(uuid)
		odbi.signalCB.OnLogicalRouterDelete(lr)
	case TableLogicalRouterPort:
		lrp := odbi.rowToLogicalRouterPort(uuid)
		odbi.signalCB.OnLogicalRouterPortDelete(lrp)
	case TableLogicalRouterStaticRoute:
		lrsr := odbi.rowToLogicalRouterStaticRoute(uuid)
		odbi.signalCB.OnLogicalRouterStaticRouteDelete(lrsr)
	case TableLogicalSwitch:
		ls := odbi.rowToLogicalSwitch(uuid)
		odbi.signalCB.OnLogicalSwitchDelete(ls)
	case TableLogicalSwitchPort:
		lp, err := odbi.uuidToLogicalPort(uuid)
		if err == nil {
			odbi.signalCB.OnLogicalPortDelete(lp)
		}
	case TableACL:
		acl := odbi.rowToACL(uuid)
		odbi.signalCB.OnACLDelete(acl)
	case TableDHCPOptions:
		dhcp := odbi.rowToDHCPOptions(uuid)
		odbi.signalCB.OnDHCPOptionsDelete(dhcp)
	case TableQoS:
		qos := odbi.rowToQoS(uuid)
		odbi.signalCB.OnQoSDelete(qos)
	case TableLoadBalancer:
		lb, _ := odbi.rowToLB(uuid)
		odbi.signalCB.OnLoadBalancerDelete(lb)
	case TableMeter:
		meter := odbi.rowToMeter(uuid)
		odbi.signalCB.OnMeterDelete(meter)
	case TableMeterBand:
		band, _ := odbi.rowToMeterBand(uuid)
		odbi.signalCB.OnMeterBandDelete(band)
	case TableChassis:
		chassis, _ := odbi.rowToChassis(uuid)
		odbi.signalCB.OnChassisDelete(chassis)
	case TableEncap:
		encap, _ := odbi.rowToEncap(uuid)
		odbi.signalCB.OnEncapDelete(encap)
	}
}

func (odbi *ovndb) requestDisconnect() {
	select {
	case odbi.disconnSig <- struct{}{}:
		klog.V(5).Infof("Requested disconnect from follower")
	default:
		klog.V(5).Infof("Disconnect from follower already requested")
	}
}

func (odbi *ovndb) disconnectIfFollower(table, uuid string) {
	if table == TableDatabase && odbi.leaderOnly && !odbi.serverIsLeader() {
		klog.Infof("Leader-only requested; disconnecting from follower %s...", odbi.endpoints[odbi.curEndpoint])
		// Disconnect client and let the disconnect notification
		// from libovsdb trigger our reconnect handler
		odbi.nextEndpoint()
		odbi.requestDisconnect()
	}
}

func (odbi *ovndb) getContext(dbName string) (*map[string][]string, *map[string]map[string]libovsdb.Row, func(string, string), func(string, string)) {
	if dbName == DBServer {
		return &odbi.serverTableCols, &odbi.serverCache, odbi.disconnectIfFollower, odbi.disconnectIfFollower
	}
	if odbi.signalCB == nil {
		return &odbi.tableCols, &odbi.cache, nil, nil
	}
	return &odbi.tableCols, &odbi.cache, odbi.signalCreate, odbi.signalDelete
}

func (odbi *ovndb) populateCache(dbName string, updates libovsdb.TableUpdates, signal bool) {
	tableCols, cache, signalCreate, signalDelete := odbi.getContext(dbName)

	empty := libovsdb.Row{}

	for table := range *tableCols {
		tableUpdate, ok := updates.Updates[table]
		if !ok {
			continue
		}

		if _, ok := (*cache)[table]; !ok {
			(*cache)[table] = make(map[string]libovsdb.Row)
		}
		for uuid, row := range tableUpdate.Rows {
			// TODO: this is a workaround for the problem of
			// missing json number conversion in libovsdb
			odbi.float64_to_int(row.New)

			if !reflect.DeepEqual(row.New, empty) {
				if reflect.DeepEqual(row.New, (*cache)[table][uuid]) {
					// Already existed and unchanged, ignore (this can happen when auto-reconnect)
					continue
				}
				(*cache)[table][uuid] = row.New
				if signal && signalCreate != nil {
					signalCreate(table, uuid)
				}
			} else {
				defer delete((*cache)[table], uuid)
				if signal && signalDelete != nil {
					defer signalDelete(table, uuid)
				}
			}
		}
	}
}

func (odbi *ovndb) initMissingColumnsWithDefaults(db, table string, row *libovsdb.Row) {
	schema := odbi.getSchema(db)
	tableSchema := schema.Tables[table]

	for column, columnSchema := range tableSchema.Columns {
		_, ok := row.Fields[column]
		if !ok {
			switch columnSchema.Type {
			case "integer", "real":
				row.Fields[column] = columnSchema.TypeObj.Min()
			case "boolean":
				if (columnSchema.TypeObj.Min() == 0) && (columnSchema.TypeObj.Max() == 1) {
					row.Fields[column] = interface{}(nil)
				} else {
					row.Fields[column] = false
				}
			case "map":
				row.Fields[column] = libovsdb.OvsMap{GoMap: make(map[interface{}]interface{})}
			case "set":
				row.Fields[column] = libovsdb.OvsSet{GoSet: make([]interface{}, 0)}
			case "string":
				row.Fields[column] = ""
			default:
				row.Fields[column] = interface{}(nil)
			}
		}
	}
}

func updateSetWithElem(ovsSet *libovsdb.OvsSet, elem interface{}) {
	bv := reflect.ValueOf(ovsSet.GoSet)
	nv := reflect.ValueOf(elem)
	var found bool
	for i := 0; i < bv.Len(); i++ {
		if bv.Index(i).Interface() == nv.Interface() {
			// found a match, delete from slice
			found = true
			ovsSet.GoSet[i] = ovsSet.GoSet[bv.Len()-1]
			ovsSet.GoSet = ovsSet.GoSet[0 : bv.Len()-1]
			break
		}
	}

	if !found {
		ovsSet.GoSet = append(ovsSet.GoSet, elem)
	}
}

func (odbi *ovndb) modifySet(orig interface{}, elem interface{}) *libovsdb.OvsSet {
	var cv *libovsdb.OvsSet
	o := reflect.ValueOf(orig)
	switch o.Elem().Interface().(type) {
	case libovsdb.OvsSet:
		t := o.Elem().Interface().(libovsdb.OvsSet)
		cv = &t
	default:
		temp := o.Elem().Interface()
		origC := make([]interface{}, 1)
		origC[0] = temp
		cv, _ = libovsdb.NewOvsSet(origC)
	}

	switch elem.(type) {
	case libovsdb.OvsSet:
		t := elem.(libovsdb.OvsSet)
		tv := reflect.ValueOf(t.GoSet)
		for i := 0; i < tv.Len(); i++ {
			updateSetWithElem(cv, tv.Index(i).Interface())
		}
	default:
		updateSetWithElem(cv, elem)
	}

	return cv
}

func (odbi *ovndb) applyUpdatesToRow(db, table string, uuid string, rowdiff *libovsdb.Row, cache *map[string]map[string]libovsdb.Row) {
	row := (*cache)[table][uuid]

	for column, value := range rowdiff.Fields {
		columnSchema, ok := odbi.getSchema(db).Tables[table].Columns[column]
		if !ok {
			continue
		}

		switch columnSchema.Type {
		case "map":
			for k, v := range value.(libovsdb.OvsMap).GoMap {
				pv, ok := row.Fields[column].(libovsdb.OvsMap).GoMap[k]
				if !ok {
					/* New key */
					row.Fields[column].(libovsdb.OvsMap).GoMap[k] = v
				} else {
					if pv != v {
						/* Value changed.  Update it. */
						row.Fields[column].(libovsdb.OvsMap).GoMap[k] = v
					} else {
						/* Delete the key. */
						delete(row.Fields[column].(libovsdb.OvsMap).GoMap, k)
					}
				}
			}
		case "set":
			if columnSchema.TypeObj.Max() == 1 {
				row.Fields[column] = value
			} else {
				cv := row.Fields[column]
				nv := odbi.modifySet(&cv, value)
				bv := reflect.ValueOf(nv.GoSet)
				if bv.Len() == 1 {
					row.Fields[column] = bv.Index(0).Interface()
				} else {
					row.Fields[column] = *nv
				}
			}
		default:
			row.Fields[column] = value
		}
	}

	(*cache)[table][uuid] = row
}

func (odbi *ovndb) populateCache2(dbName string, updates libovsdb.TableUpdates2, signal bool) {
	tableCols, cache, signalCreate, signalDelete := odbi.getContext(dbName)

	for table := range *tableCols {
		tableUpdate, ok := updates.Updates[table]
		if !ok {
			continue
		}

		if _, ok := (*cache)[table]; !ok {
			(*cache)[table] = make(map[string]libovsdb.Row)
		}

		for uuid, row := range tableUpdate.Rows {
			switch {
			case row.Initial.Fields != nil:
				// TODO: this is a workaround for the problem of
				// missing json number conversion in libovsdb
				odbi.float64_to_int(row.Initial)
				if reflect.DeepEqual(row.Initial, (*cache)[table][uuid]) {
					// Already existed and unchanged, ignore (this can happen when auto-reconnect)
					continue
				}
				odbi.initMissingColumnsWithDefaults(dbName, table, &row.Initial)
				(*cache)[table][uuid] = row.Initial
				if signal && signalCreate != nil {
					signalCreate(table, uuid)
				}
			case row.Insert.Fields != nil:
				odbi.initMissingColumnsWithDefaults(dbName, table, &row.Insert)
				// TODO: this is a workaround for the problem of
				// missing json number conversion in libovsdb
				odbi.float64_to_int(row.Insert)
				(*cache)[table][uuid] = row.Insert
				if signal && signalCreate != nil {
					signalCreate(table, uuid)
				}
			case row.Modify.Fields != nil:
				// TODO: this is a workaround for the problem of
				// missing json number conversion in libovsdb
				odbi.float64_to_int(row.Modify)
				odbi.applyUpdatesToRow(dbName, table, uuid, &row.Modify, cache)
				if signal && signalCreate != nil {
					signalCreate(table, uuid)
				}
			case row.Delete.Fields != nil:
				defer delete((*cache)[table], uuid)
				if signal && signalDelete != nil {
					signalDelete(table, uuid)
				}
			}
		}
	}
}

func (odbi *ovndb) ConvertGoSetToStringArray(oset libovsdb.OvsSet) []string {
	var ret = []string{}
	for _, s := range oset.GoSet {
		switch s.(type) {
		case string:
			value := s.(string)
			ret = append(ret, value)
		case libovsdb.UUID:
			uuid := s.(libovsdb.UUID)
			ret = append(ret, uuid.GoUUID)
		}
	}
	return ret
}

func (odbi *ovndb) optionalStringFieldToPointer(fieldValue interface{}) *string {
	switch fieldValue.(type) {
	case string:
		temp := fieldValue.(string)
		return &temp
	case libovsdb.OvsSet:
		temp := odbi.ConvertGoSetToStringArray(fieldValue.(libovsdb.OvsSet))
		if len(temp) > 0 {
			return &temp[0]
		}
		return nil
	}
	return nil
}

func stringToGoUUID(uuid string) libovsdb.UUID {
	return libovsdb.UUID{GoUUID: uuid}
}

func (odbi *ovndb) auxKeyValSet(table string, rowName string, auxCol string, kv map[string]string) (*OvnCommand, error) {
	if len(kv) == 0 {
		return nil, fmt.Errorf("key-value map is nil or empty")
	}

	ovnRow := make(OVNRow)
	ovnRow["name"] = rowName

	uuid := odbi.getRowUUID(table, ovnRow)
	col := odbi.cache[table][uuid].Fields[auxCol]
	if col == nil {
		return nil, fmt.Errorf("table %s, row %s, column %s not present in cache", table, rowName, auxCol)
	}

	switch col.(type) {
	case libovsdb.OvsMap:
	default:
		return nil, fmt.Errorf("table %s, row %s, column %s: value is not a map", table, rowName, auxCol)
	}

	cachedMap := col.(libovsdb.OvsMap).GoMap

	// prepare new map for the update by copying keys/values from the kv map,
	// followed by copying all other keys/values from the cache. NB: this is to implement functionality
	// not explicitly provided by RFC7047 - change values for individual keys that already exist
	mergedMap := make(map[interface{}]interface{}, len(kv)+len(cachedMap))
	for k, v := range kv {
		mergedMap[k] = v
	}
	for k, v := range cachedMap {
		ck := k.(string)
		if _, ok := kv[ck]; !ok {
			mergedMap[ck] = v.(string)
		}
	}

	auxMap, err := libovsdb.NewOvsMap(mergedMap)
	if err != nil {
		return nil, err
	}
	ovnRow[auxCol] = auxMap

	condition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(uuid))
	operation := libovsdb.Operation{
		Op:    opUpdate,
		Table: table,
		Where: []interface{}{condition},
		Row:   ovnRow,
	}

	operations := []libovsdb.Operation{operation}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) auxKeyValDel(table string, rowName string, auxCol string, kv map[string]*string) (*OvnCommand, error) {
	if len(kv) == 0 {
		return nil, fmt.Errorf("KV map is empty")
	}

	ovnRow := make(OVNRow)
	ovnRow["name"] = rowName
	uuid := odbi.getRowUUID(TableLogicalSwitch, ovnRow)
	if len(uuid) == 0 {
		return nil, ErrorNotFound
	}

	delKeys := []string{}
	delKeyVals := make(map[string]string, len(kv))

	for k, v := range kv {
		if v == nil {
			delKeys = append(delKeys, k)
		} else {
			delKeyVals[k] = *v
		}
	}

	var mutateSet *libovsdb.OvsSet
	var mutateMap *libovsdb.OvsMap
	var err error

	condition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(uuid))
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     table,
		Mutations: []interface{}{},
		Where:     []interface{}{condition},
	}

	if len(delKeys) != 0 {
		mutateSet, err = libovsdb.NewOvsSet(delKeys)
		if err != nil {
			return nil, err
		}
		m := libovsdb.NewMutation(auxCol, opDelete, mutateSet)
		mutateOp.Mutations = append(mutateOp.Mutations, m)
	}
	if len(delKeyVals) != 0 {
		mutateMap, err = libovsdb.NewOvsMap(delKeyVals)
		if err != nil {
			return nil, err
		}
		m := libovsdb.NewMutation(auxCol, opDelete, mutateMap)
		mutateOp.Mutations = append(mutateOp.Mutations, m)
	}

	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}
