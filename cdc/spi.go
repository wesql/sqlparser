/*
Copyright ApeCloud, Inc.
Licensed under the Apache v2(found in the LICENSE file in the root directory).
*/
package cdc

import (
	querypb "github.com/wesql/sqlparser/go/vt/proto/query"
)

var SpiOpen = func(cc *CdcConsumer) {

}

var SpiLoadGTIDAndLastPK = func(cc *CdcConsumer) (string, *querypb.QueryResult, error) {
	return "", nil, nil
}

var SpiStoreGtidAndLastPK = func(currentGTID string, currentPK *querypb.QueryResult, cc *CdcConsumer) error {
	return nil
}

var SpiStoreTableData = func(resultList []*RowResult, colInfoMap map[string]*ColumnInfo, pkFields []*querypb.Field, cc *CdcConsumer) error {
	return nil
}

var SpiClose = func(cc *CdcConsumer) {

}
