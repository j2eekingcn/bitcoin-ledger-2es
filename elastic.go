package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/olivere/elastic"
	"github.com/shopspring/decimal"
	log "github.com/sirupsen/logrus"
)

type elasticClientAlias struct {
	*elastic.Client
}

func (conf configure) elasticClient() (*elasticClientAlias, error) {
	client, err := elastic.NewClient(elastic.SetURL(conf.ElasticURL),
		elastic.SetSniff(conf.ElasticSniff))
	if err != nil {
		return nil, err
	}
	elasticClient := elasticClientAlias{client}
	return &elasticClient, nil
}

func (client *elasticClientAlias) createIndices() {
	ctx := context.Background()
	for _, index := range []string{"block", "tx", "vout", "balance"} {
		var mapping string
		switch index {
		case "block":
			mapping = blockMapping
		case "tx":
			mapping = txMapping
		case "vout":
			mapping = voutMapping
		case "balance":
			mapping = balanceMapping
		}
		result, err := client.CreateIndex(index).BodyString(mapping).Do(ctx)
		if err != nil {
			continue
		}
		if result.Acknowledged {
			log.Infoln(strings.Join([]string{"Create index:", result.Index}, ""))
		}
	}
}

func (client *elasticClientAlias) MaxAgg(field, index, typeName string) (*float64, error) {
	ctx := context.Background()
	hightestAgg := elastic.NewMaxAggregation().Field(field)
	aggKey := strings.Join([]string{"max", field}, "_")
	// Get Query params https://github.com/olivere/elastic/blob/release-branch.v6/search_aggs_metrics_max_test.go
	// https://www.elastic.co/guide/en/elasticsearch/reference/6.2/search-aggregations-metrics-max-aggregation.html
	searchResult, err := client.Search().
		Index(index).Type(typeName).
		Query(elastic.NewMatchAllQuery()).
		Aggregation(aggKey, hightestAgg).
		Do(ctx)

	if err != nil {
		return nil, err
	}
	maxAggRes, found := searchResult.Aggregations.Max(aggKey)
	if !found || maxAggRes.Value == nil {
		return nil, errors.New(strings.Join([]string{"max", field, "in", index, typeName, "not found"}, " "))
	}
	return maxAggRes.Value, nil
}

func (client *elasticClientAlias) QueryVoutWithVinsOrVouts(ctx context.Context, IndexUTXOs []IndexUTXO) ([]*VoutWithID, error) {
	q := elastic.NewBoolQuery()
	for _, vin := range IndexUTXOs {
		qnestedBool := elastic.NewBoolQuery()
		qnestedBool.Must(elastic.NewTermQuery("txidbelongto", vin.Txid), elastic.NewTermQuery("voutindex", vin.Index))
		q.Should(qnestedBool)
	}
	searchResult, err := client.Search().Index("vout").Type("vout").Size(len(IndexUTXOs)).Query(q).Do(ctx)
	if err != nil {
		return nil, errors.New(strings.Join([]string{"query vouts error:", err.Error()}, ""))
	}
	var voutWithIDs []*VoutWithID
	for _, vout := range searchResult.Hits.Hits {
		newVout := new(VoutStream)
		if err := json.Unmarshal(*vout.Source, newVout); err != nil {
			log.Fatalln("query vouts error: unmarshal json ", err.Error())
		}
		voutWithIDs = append(voutWithIDs, &VoutWithID{vout.Id, newVout})
	}
	return voutWithIDs, nil
}

// FindVoutByVinIndexAndTxID 根据 vin 的 txid 和 vout 字段, 从 voutstream 找出 vout
func (client *elasticClientAlias) FindVoutByVoutIndexAndBelongTxID(ctx context.Context, txidbelongto string, voutindex uint32) (*string, *VoutStream, error) {
	// https://github.com/olivere/elastic/wiki/QueryDSL
	// https://www.elastic.co/guide/en/elasticsearch/reference/current/query-dsl-bool-query.html
	// etc curl -XGET 'http://192.168.99.100:32776/btc-mainnet/_search?pretty' -d ' {"query":{"bool":{"must":[{"term":{"txidbelongto":"df2b060fa2e5e9c8ed5eaf6a45c13753ec8c63282b2688322eba40cd98ea067a"}},{"term":{"voutindex":0}}]}}}'
	q := elastic.NewBoolQuery()

	// 根据 vin 的 txid 和 vout 字段, 从 voutstream 找出 vout
	q = q.Must(elastic.NewTermQuery("txidbelongto", txidbelongto))
	q = q.Must(elastic.NewTermQuery("voutindex", voutindex))
	searchResult, err := client.Search().Index("vout").Type("vout").Query(q).Do(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(searchResult.Hits.Hits) < 1 {
		return nil, nil, errors.New("vout not found by the condition")
	}
	hit := searchResult.Hits.Hits[0]
	vout := new(VoutStream)
	if err := json.Unmarshal(*hit.Source, vout); err != nil {
		log.Fatalln(err.Error())
	}
	return &(hit.Id), vout, nil
}

func (client *elasticClientAlias) FindEsBlockByHeight(ctx context.Context, height int32) (*btcjson.GetBlockVerboseResult, error) {
	blockHeightStr := strconv.FormatInt(int64(height), 10)
	res, err := client.Get().Index("block").Type("block").Id(blockHeightStr).Refresh("true").Do(ctx)
	if err != nil {
		return nil, err
	}
	if !res.Found {
		return nil, errors.New(strings.Join([]string{"block:", blockHeightStr, "not fount in es when update txstream"}, ""))
	}
	NewBlock := new(btcjson.GetBlockVerboseResult)
	err = json.Unmarshal(*res.Source, NewBlock)
	if err != nil {
		return nil, err
	}
	return NewBlock, nil
}

// FindVoutByUsedFieldAndBelongTxID 根据 used object 和所在交易 ID 在 voutStream type 中查找 vout
func (client *elasticClientAlias) FindVoutByUsedFieldAndBelongTxID(ctx context.Context, vin btcjson.Vin, txBelongto string) (*string, *VoutStream, error) {
	bq := elastic.NewBoolQuery()
	bq = bq.Must(elastic.NewTermQuery("txidbelongto", vin.Txid))  // voutStream 所在的交易 ID 属于 vin 的 TxID 字段
	bq = bq.Must(elastic.NewTermQuery("used.txid", txBelongto))   // vin 所在的交易 ID 属于 voutStream used object 中的 txid 字段
	bq = bq.Must(elastic.NewTermQuery("used.vinindex", vin.Vout)) // vin 所在的交易输入索引属于 voutStream used object 中的 vinindex 字段

	// bqsrc, _ := bq.Source()
	// bqdata, _ := json.Marshal(bqsrc)
	// fmt.Println(string(bqdata))

	searchResult, err := client.Search().Index("vout").Type("vout").Query(bq).Do(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(searchResult.Hits.Hits) < 1 {
		return nil, nil, errors.New("vout not found by the condition")
	}
	hit := searchResult.Hits.Hits[0]
	vout := new(VoutStream)
	if err := json.Unmarshal(*hit.Source, vout); err != nil {
		log.Fatalln(err.Error())
	}
	return &(hit.Id), vout, nil
}

// FindVoutsByUsedFieldAndBelongTxID 根据 vins 的 used object 和所在交易 ID 在 voutStream type 中查找 vouts ids
func (client *elasticClientAlias) QueryVoutsByUsedFieldAndBelongTxID(ctx context.Context, vins []btcjson.Vin, txBelongto string) ([]*VoutWithID, error) {
	if len(vins) == 1 && len(vins[0].Coinbase) != 0 && len(vins[0].Txid) == 0 {
		return nil, errors.New("coinbase tx, vin is new and not exist in es vout Type")
	}
	var esVoutIDS []string

	q := elastic.NewBoolQuery()
	for _, vin := range vins {
		bq := elastic.NewBoolQuery()
		bq = bq.Must(elastic.NewTermQuery("txidbelongto", vin.Txid))  // voutStream 所在的交易 ID 属于 vin 的 TxID 字段
		bq = bq.Must(elastic.NewTermQuery("used.txid", txBelongto))   // vin 所在的交易 ID 属于 voutStream used object 中的 txid 字段
		bq = bq.Must(elastic.NewTermQuery("used.vinindex", vin.Vout)) // vin 所在的交易输入索引属于 voutStream used object 中的 vinindex 字段
		q.Should(bq)
	}

	searchResult, err := client.Search().Index("vout").Type("vout").Size(len(vins)).Query(q).Do(ctx)
	if err != nil {
		return nil, err
	}
	if len(searchResult.Hits.Hits) < 1 {
		return nil, errors.New("vout not found by the condition")
	}

	if len(searchResult.Hits.Hits) != len(vins) {
		return nil, errors.New("vins")
	}

	var voutWithIDs []*VoutWithID
	for _, rawHit := range searchResult.Hits.Hits {
		newVout := new(VoutStream)
		if err := json.Unmarshal(*rawHit.Source, newVout); err != nil {
			log.Fatalln(err.Error())
		}
		esVoutIDS = append(esVoutIDS, rawHit.Id)
		voutWithIDs = append(voutWithIDs, &VoutWithID{rawHit.Id, newVout})
	}
	return voutWithIDs, nil
}

func (client *elasticClientAlias) FindBalanceWithAddressOrInitWithAmount(ctx context.Context, address string, amount float64) (*string, *Balance, error) {
	q := elastic.NewTermQuery("address", address)
	searchResult, err := client.Search().Index("balance").Type("balance").Query(q).Do(ctx)
	if err != nil {
		return nil, nil, err
	}

	var balance = new(Balance)
	if len(searchResult.Hits.Hits) < 1 {
		balance.Address = address
		balance.Amount = amount
		return nil, balance, errors.New(strings.Join([]string{address, "not found in balance type"}, " "))
	}
	hit := searchResult.Hits.Hits[0]
	err = json.Unmarshal(*hit.Source, balance)
	if err != nil {
		log.Fatalln(err.Error())
	}
	return &(hit.Id), balance, nil
}

func (client *elasticClientAlias) UpdateBTCBlance(ctx context.Context, operateType, id string, btcbalance *Balance, amount float64) error {
	balance := decimal.NewFromFloat(btcbalance.Amount)
	switch operateType {
	case "add":
		balance = balance.Add(decimal.NewFromFloat(amount))
	case "sub":
		balance = balance.Sub(decimal.NewFromFloat(amount))
	default:
		return errors.New("operateType params error, it's value is one of the 'add' or sub'")
	}
	balanceToFloat, _ := balance.Float64()
	_, err := client.Update().Index("balance").Type("balance").Id(id).Doc(map[string]interface{}{"amount": balanceToFloat}).DocAsUpsert(true).Refresh("true").Do(ctx)
	if err != nil {
		log.Fatalln("update btcbalance docutment error:", id, err.Error())
	}
	return nil
}

func (client *elasticClientAlias) UpdateVoutUsedField(ctx context.Context, id string, vinBelongTxid string, vin btcjson.Vin) {
	// 更新 voutStream 的 used 字段，该字段数据类型为 object, txid 为 vin 所属 tx 的 txid, vinindex 为 vin 在所属 tx 中的 vins 序号
	client.Update().Index("vout").Type("vout").Id(id).Doc(map[string]interface{}{"used": voutUsed{Txid: vinBelongTxid, VinIndex: vin.Vout}}).
		DocAsUpsert(true).DetectNoop(true).Refresh("true").Do(ctx)
}

func (client *elasticClientAlias) RollbackTxVoutBalanceTypeByBlockHeight(ctx context.Context, height int32) error {
	bulkRequest := client.Bulk()
	var (
		vinAddresses                      []interface{} // All addresses related with vins in a block
		voutAddresses                     []interface{} // All addresses related with vouts in a block
		vinAddressWithAmountSlice         []*Balance
		voutAddressWithAmountSlice        []*Balance
		UniqueVinAddressesWithSumWithdraw []*AddressWithAmount // 统计区块中所有 vout 涉及到去重后的 vout 地址及其对应的增加余额
		UniqueVoutAddressesWithSumDeposit []*AddressWithAmount // 统计区块中所有 vout 涉及到去重后的 vout 地址及其对应的增加余额
		vinBalancesWithIDs                []*BalanceWithID
		voutBalancesWithIDs               []*BalanceWithID
	)

	NewBlock, err := client.FindEsBlockByHeight(ctx, height)
	if err != nil {
		return err
	}

	// rollback: delete txs in es by block hash
	if err := client.DeleteEsTxsByBlockHash(ctx, NewBlock.Hash); err != nil {
		return err
	}

	for _, tx := range NewBlock.Tx {
		voutWithIDSliceForVins, err := client.QueryVoutsByUsedFieldAndBelongTxID(ctx, tx.Vin, tx.Txid)
		if err != nil {
			log.Warnln(err.Error())
		}
		for _, voutWithID := range voutWithIDSliceForVins {
			// rollback: update vout's used to nil
			updateVoutUsedField := elastic.NewBulkUpdateRequest().Index("vout").Type("vout").Id(voutWithID.ID).
				Doc(map[string]interface{}{"used": nil})
			bulkRequest.Add(updateVoutUsedField).Refresh("true")

			_, vinAddressesTmp, vinAddressWithAmountSliceTmp := parseESVout(voutWithID)
			vinAddresses = append(vinAddresses, vinAddressesTmp...)
			vinAddressWithAmountSlice = append(vinAddressWithAmountSlice, vinAddressWithAmountSliceTmp...)
		}

		// get es vouts with id in elasticsearch by tx vouts
		indexVouts := indexedVoutsFun(tx.Vout, tx.Txid)
		voutWithIDSliceForVouts, err := client.QueryVoutWithVinsOrVouts(ctx, indexVouts)
		if err != nil {
			log.Fatalln("QueryVoutWithVinsOrVouts error: vout not found", err.Error())
		}
		for _, voutWithID := range voutWithIDSliceForVouts {
			// rollback: delete vout
			deleteVout := elastic.NewBulkDeleteRequest().Index("vout").Type("vout").Id(voutWithID.ID)
			bulkRequest.Add(deleteVout)

			_, voutAddressesTmp, voutAddressWithAmountSliceTmp := parseESVout(voutWithID)
			voutAddresses = append(voutAddresses, voutAddressesTmp...)
			voutAddressWithAmountSlice = append(voutAddressWithAmountSlice, voutAddressWithAmountSliceTmp...)
		}

		for _, vin := range tx.Vin {
			if len(tx.Vin) == 1 && len(tx.Vin[0].Coinbase) != 0 && len(tx.Vin[0].Txid) == 0 {
				continue // the vin is coinbase
			}
			if voutID, DBVout, err := client.FindVoutByUsedFieldAndBelongTxID(ctx, vin, tx.Txid); err != nil {
				fmt.Println(err.Error())
			} else {
				// rollback voutStream used object field
				// client.Update().Index("vout").Type("vout").Id(*voutID).Doc(map[string]interface{}{"used": nil}).
				// 	DocAsUpsert(true).DetectNoop(true).Refresh("true").Do(ctx)
				fmt.Println("rollback vout", *voutID, "used object field as null")

				// arollback balance: add
				client.UpdateBTCBlanceByVout(ctx, DBVout, "add")
			}
		}

		for _, vout := range tx.Vout {
			// 根据 vout 所在的 txid 和 vout 的 N 字段, 从 voutstream 找出 vout
			voutUsedID, DBVout, err := client.FindVoutByVoutIndexAndBelongTxID(ctx, tx.Txid, vout.N)
			if err != nil {
				fmt.Println("voutstream rollback fail:", err.Error())
				continue
			}
			// rollback voutStream : delete the vout
			// client.Delete().Index("vout").Type("vout").Id(*voutUsedID).Refresh("true").Do(ctx)
			fmt.Println("rollback vout", *voutUsedID, "deleted", DBVout.TxIDBelongTo)

			// arollback balance: sub
			client.UpdateBTCBlanceByVout(ctx, DBVout, "sub")
		}
	}

	// 统计块中所有交易 vin 涉及到的地址及其对应的提现余额 (balance type)
	UniqueVinAddressesWithSumWithdraw = calculateUniqueAddressWithSumForVinOrVout(vinAddresses, vinAddressWithAmountSlice)
	bulkQueryVinBalance, err := client.BulkQueryBalance(ctx, vinAddresses...)
	if err != nil {
		log.Fatalln(err.Error())
	}
	vinBalancesWithIDs = bulkQueryVinBalance

	// 统计块中所有交易 vin 涉及到的地址及其对应的提现余额 (balance type)
	UniqueVoutAddressesWithSumDeposit = calculateUniqueAddressWithSumForVinOrVout(voutAddresses, voutAddressWithAmountSlice)
	bulkQueryVoutBalance, err := client.BulkQueryBalance(ctx, voutAddresses...)
	if err != nil {
		log.Fatalln(err.Error())
	}
	voutBalancesWithIDs = bulkQueryVoutBalance

	// rollback: add to addresses related to vins addresses
	bulkUpdateVinBalanceRequest := client.Bulk()
	// update(sub)  balances related to vins addresses
	// len(vinAddressWithSumWithdraw) == len(vinBalancesWithIDs)
	for _, vinAddressWithSumWithdraw := range UniqueVinAddressesWithSumWithdraw {
		for _, vinBalanceWithID := range vinBalancesWithIDs {
			if vinAddressWithSumWithdraw.Address == vinBalanceWithID.Balance.Address {
				balance := decimal.NewFromFloat(vinBalanceWithID.Balance.Amount).Add(vinAddressWithSumWithdraw.Amount)
				amount, _ := balance.Float64()
				updateVinBalcne := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(vinBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkUpdateVinBalanceRequest.Add(updateVinBalcne).Refresh("true")
				break
			}
		}
	}
	if bulkUpdateVinBalanceRequest.NumberOfActions() != 0 {
		bulkUpdateVinBalanceResp, e := bulkUpdateVinBalanceRequest.Refresh("true").Do(ctx)
		if e != nil {
			log.Fatalln(err.Error())
		}
		bulkUpdateVinBalanceResp.Updated()
	}

	// update(sub) balances related to vouts addresses
	// len(voutAddressWithSumDeposit) >= len(voutBalanceWithID)
	for _, voutAddressWithSumDeposit := range UniqueVoutAddressesWithSumDeposit {
		for _, voutBalanceWithID := range voutBalancesWithIDs {
			if voutAddressWithSumDeposit.Address == voutBalanceWithID.Balance.Address {
				balance := decimal.NewFromFloat(voutBalanceWithID.Balance.Amount).Sub(voutAddressWithSumDeposit.Amount)
				amount, _ := balance.Float64()
				if amount < 0 {
					log.Fatalln("rollback vout balance error")
				}
				updateVinBalcne := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(voutBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkUpdateVinBalanceRequest.Add(updateVinBalcne).Refresh("true")
				break
			}
		}
	}

	bulkResp, err := bulkRequest.Refresh("true").Do(ctx)
	if err != nil {
		log.Fatalln(err.Error())
	}
	bulkResp.Updated()
	bulkResp.Deleted()
	bulkResp.Indexed()

	return nil
}

func (client *elasticClientAlias) DeleteEsTxsByBlockHash(ctx context.Context, blockHash string) error {
	q := elastic.NewTermQuery("blockhash", blockHash)
	if _, err := client.DeleteByQuery().Index("tx").Type("tx").Query(q).Refresh("true").Do(ctx); err != nil {
		return errors.New(strings.Join([]string{"Delete", blockHash, "'s all transactions from es tx type fail"}, ""))
	}
	fmt.Println("Delete all transaction in", blockHash, "from es tx type")
	return nil
}

func (client *elasticClientAlias) UpdateBTCBlanceByVout(ctx context.Context, vout *VoutStream, OperateType string) error {
	for _, address := range vout.Addresses {
		// find BTCBalance docutment by address
		if balancdID, btcbalance, err := client.FindBalanceWithAddressOrInitWithAmount(ctx, address, vout.Value); err == nil {
			if err := client.UpdateBTCBlance(ctx, OperateType, *balancdID, btcbalance, vout.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (client *elasticClientAlias) BTCRollBackAndSyncTx(from, height int32, block *btcjson.GetBlockVerboseResult, wg *sync.WaitGroup) {
	defer wg.Done()
	ctx := context.Background()
	if height < (from + 5) {
		client.RollbackTxVoutBalanceTypeByBlockHeight(ctx, height)
	}
	// client.BTCSyncTx(ctx, from, block)
	client.syncTx(ctx, block)
	client.Flush()
}

// BulkQueryBalance query balances by address slice
func (client *elasticClientAlias) BulkQueryBalance(ctx context.Context, addresses ...interface{}) ([]*BalanceWithID, error) {
	var (
		balancesWithIDs []*BalanceWithID
		qAddresses      []interface{}
	)

	uniqueAddresses := removeDuplicatesForSlice(addresses...)
	for _, address := range uniqueAddresses {
		qAddresses = append(qAddresses, address)
	}

	q := elastic.NewTermsQuery("address", qAddresses...)
	searchResult, err := client.Search().Index("balance").Type("balance").Size(len(qAddresses)).Query(q).Do(ctx)
	if err != nil {
		return nil, errors.New(strings.Join([]string{"Get balances error:", err.Error()}, " "))
	}

	for _, balance := range searchResult.Hits.Hits {
		b := new(Balance)
		if err := json.Unmarshal(*balance.Source, b); err != nil {
			return nil, errors.New(strings.Join([]string{"unmarshal error:", err.Error()}, " "))
		}
		balancesWithIDs = append(balancesWithIDs, &BalanceWithID{balance.Id, *b})
	}
	return balancesWithIDs, nil
}

// 统计块中的所有 vout 涉及到去重后的所有地址对应充值额度
func calculateUniqueAddressWithSumForVinOrVout(addresses []interface{}, AddressWithAmountSlice []*Balance) []*AddressWithAmount {
	var UniqueAddressesWithSum []*AddressWithAmount
	UniqueAddresses := removeDuplicatesForSlice(addresses...)
	// 统计去重后涉及到的 vout 地址及其对应的增加余额
	for _, uAddress := range UniqueAddresses {
		sumDeposit := decimal.NewFromFloat(0)
		for _, addressWithAmount := range AddressWithAmountSlice {
			if uAddress == addressWithAmount.Address {
				sumDeposit = sumDeposit.Add(decimal.NewFromFloat(addressWithAmount.Amount))
			}
		}
		UniqueAddressesWithSum = append(UniqueAddressesWithSum, &AddressWithAmount{uAddress, sumDeposit})
	}
	return UniqueAddressesWithSum
}
