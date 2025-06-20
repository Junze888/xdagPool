package payouts

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/XDagger/xdagpool/pool"
	"github.com/XDagger/xdagpool/util"
	xdagoUtils "github.com/XDagger/xdagpool/xdago/utils"

	"golang.org/x/exp/utf8string"

	"github.com/XDagger/xdagpool/xdago/base58"
	"github.com/XDagger/xdagpool/xdago/common"
	"github.com/XDagger/xdagpool/xdago/cryptography"
	"github.com/XDagger/xdagpool/xdago/secp256k1"
	"github.com/buger/jsonparser"
)

var Cfg *pool.Config

func xdagjRpc(method string, params string) (string, error) {
	url := Cfg.NodeRpc
	//fmt.Println(url)
	var sb strings.Builder
	sb.WriteString(`{"jsonrpc":"2.0","id":1,"method":"`)
	sb.WriteString(method)
	sb.WriteString(`","params":["`)
	sb.WriteString(params)
	sb.WriteString(`"]}`)

	//fmt.Println(sb.String())
	jsonData := []byte(sb.String())
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: 20 * time.Second,
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	util.Info.Println(string(body))
	errMsg, err := jsonparser.GetString(body, "error", "message")
	if err == nil {
		return "", errors.New(errMsg)
	}
	return jsonparser.GetString(body, "result")
}

func TransferRpc(amount float64, from, to, remark string, key *secp256k1.PrivateKey) (string, error) {
	txNonce, err := getTxNonce(from)
	if err != nil {
		return "", err
	}

	blockHexStr := transactionBlock(from, to, remark, amount, key, txNonce)
	util.Debug.Println(blockHexStr)
	if blockHexStr == "" {
		return "", errors.New("create transaction block error")
	}

	txHash := blockHash(blockHexStr)
	util.Info.Println(from, "to", to, amount, remark, "transaction:", txHash)

	hash, err := xdagjRpc("xdag_sendRawTransaction", blockHexStr)
	if err != nil {
		return "", err
	}

	if hash == "" {
		return "", errors.New("transaction rpc return empty hash")
	}

	if !ValidateXdagAddress(hash) {
		return "", errors.New(hash)
	}

	if hash != txHash {
		util.Error.Println("want", txHash, "get", hash)
		return "", errors.New("transaction block hash error")
	}

	return hash, nil
}

func BalanceRpc(address string) (string, error) {

	return xdagjRpc("xdag_getBalance", address)
}

func transactionBlock(from, to, remark string, value float64, key *secp256k1.PrivateKey, txNonce uint64) string {
	if key == nil {
		util.Error.Println("transaction default key error")
		return ""
	}
	var inAddress string
	var err error

	inAddress, err = checkBase58Address(from)
	isFromOld := err != nil
	util.Debug.Println("is old address", isFromOld)

	if isFromOld { // old xdag address
		hash, err := xdagoUtils.Address2Hash(from)
		if err != nil {
			util.Error.Println("transaction send address length error")
			return ""
		}
		inAddress = hex.EncodeToString(hash[:24])
	}

	outAddress, err := checkBase58Address(to)
	if err != nil {
		util.Error.Println(err)
		return ""
	}
	var remarkBytes [common.XDAG_FIELD_SIZE]byte
	if len(remark) > 0 {
		if ValidateRemark(remark) {
			copy(remarkBytes[:], remark)
		} else {
			util.Error.Println("remark error")
			return ""
		}
	}

	var valBytes [8]byte
	if value > 0.0 {
		transVal := xdagoUtils.Xdag2Amount(value)
		binary.LittleEndian.PutUint64(valBytes[:], transVal)
	} else {
		util.Error.Println("transaction value is zero")
		return ""
	}

	t := xdagoUtils.GetCurrentTimestamp()
	var timeBytes [8]byte
	binary.LittleEndian.PutUint64(timeBytes[:], t)

	var sb strings.Builder
	// header: transport
	sb.WriteString("0000000000000000")

	compKey := key.PubKey().SerializeCompressed()

	// header: field types
	sb.WriteString(fieldTypes(false, isFromOld,
		len(remark) > 0, compKey[0] == secp256k1.PubKeyFormatCompressedEven))

	// header: timestamp
	sb.WriteString(hex.EncodeToString(timeBytes[:]))
	// header: fee
	sb.WriteString("0000000000000000")

	// tranx_nonce
	sb.WriteString("000000000000000000000000000000000000000000000000")
	var nonceByte []byte
	binary.LittleEndian.PutUint64(nonceByte, txNonce)
	sb.WriteString(hex.EncodeToString(nonceByte))

	// input field: input address
	sb.WriteString(inAddress)
	// input field: input value
	sb.WriteString(hex.EncodeToString(valBytes[:]))
	// output field: output address
	sb.WriteString(outAddress)
	// output field: out value
	sb.WriteString(hex.EncodeToString(valBytes[:]))
	// remark field
	if len(remark) > 0 {
		sb.WriteString(hex.EncodeToString(remarkBytes[:]))
	}
	// public key field
	sb.WriteString(hex.EncodeToString(compKey[1:33]))

	r, s := transactionSign(sb.String(), key, len(remark) > 0)
	// sign field: sign_r
	sb.WriteString(r)
	// sign field: sign_s
	sb.WriteString(s)
	// zero fields
	if len(remark) > 0 {
		for i := 0; i < 16; i++ {
			sb.WriteString("00000000000000000000000000000000")
		}
	} else {
		for i := 0; i < 18; i++ {
			sb.WriteString("00000000000000000000000000000000")
		}
	}
	return sb.String()
}

func checkBase58Address(address string) (string, error) {
	addrBytes, _, err := base58.ChkDec(address)
	if err != nil {
		util.Error.Println(err)
		return "", err
	}
	if len(addrBytes) != 24 {
		util.Error.Println("transaction receive address length error")
		return "", errors.New("transaction receive address length error")
	}
	reverse(addrBytes[:20])
	return "00000000" + hex.EncodeToString(addrBytes[:20]), nil
}

func reverse(input []byte) {
	inputLen := len(input)
	inputMid := inputLen / 2

	for i := 0; i < inputMid; i++ {
		j := inputLen - i - 1

		input[i], input[j] = input[j], input[i]
	}
}

func transactionSign(block string, key *secp256k1.PrivateKey, hasRemark bool) (string, string) {
	var sb strings.Builder
	sb.WriteString(block)
	if hasRemark {
		for i := 0; i < 20; i++ {
			sb.WriteString("00000000000000000000000000000000")
		}
	} else {
		for i := 0; i < 22; i++ {
			sb.WriteString("00000000000000000000000000000000")
		}
	}

	pubKey := key.PubKey().SerializeCompressed()
	sb.WriteString(hex.EncodeToString(pubKey[:]))

	b, _ := hex.DecodeString(sb.String())

	hash := cryptography.HashTwice(b)

	r, s := cryptography.EcdsaSign(key, hash[:])
	util.Debug.Println("Sign")
	return hex.EncodeToString(r[:]), hex.EncodeToString(s[:])
}

func fieldTypes(isTest, isFromOld, hasRemark, isPubKeyEven bool) string {

	// 1/8--E--2/C--D--[9]--6/7--5--5
	// header(main/test)--tranx_nonce--input(old/new)--output--[remark]--pubKey(even/odd)--sign_r--sign_s
	var sb strings.Builder

	sb.WriteString("E") // tranx_nonce

	if isTest {
		sb.WriteString("8") // test net
	} else {
		sb.WriteString("1") // main net
	}

	sb.WriteString("D") // output

	if isFromOld {
		sb.WriteString("2") // old address
	} else {
		sb.WriteString("C") // new address
	}

	if hasRemark { // with remark
		if isPubKeyEven {
			sb.WriteString("6") // even public key
		} else {
			sb.WriteString("7") // odd public key
		}
		sb.WriteString("95500000000") // remark & signs
	} else { // without remark

		sb.WriteString("5") // sign

		if isPubKeyEven {
			sb.WriteString("6") // even public key
		} else {
			sb.WriteString("7") // odd public key
		}
		sb.WriteString("0500000000") //sign
	}

	return sb.String()
}

func blockHash(block string) string {
	b, _ := hex.DecodeString(block)
	hash := cryptography.HashTwice(b)
	return xdagoUtils.Hash2Address(hash)
}

func AddressWithBalance(addresses []string) []string {
	var res []string
	for _, addr := range addresses {
		value, err := BalanceRpc(addr)
		if err != nil {
			util.Error.Println(err)
			continue
		}
		_, err = strconv.ParseFloat(value, 64)
		if err != nil {
			util.Error.Println(err)
			continue
		}
		res = append(res, addr)
	}
	return res
}

func ValidateBipAddress(address string) bool {
	_, err := checkBase58Address(address)
	return err == nil
}

func ValidateRemark(remark string) bool {
	return utf8string.NewString(remark).IsASCII() && len(remark) <= 32
}

func ValidateXdagAddress(address string) bool {
	_, err := xdagoUtils.Address2Hash(address)
	return err == nil
}

func getTxNonce(address string) (uint64, error) {
	nonceStr, err := xdagjRpc("xdag_getTransactionNonce", address)
	if err != nil {
		util.Error.Println("get transaction nonce error", err)
		return 0, err
	}
	nonce, err := strconv.ParseUint(nonceStr, 10, 64)
	if err != nil {
		util.Error.Println("parse transaction nonce error", err)
		return 0, err
	}
	return nonce, nil
}
