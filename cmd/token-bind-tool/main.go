package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/accounts/usbwallet"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/bep20"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/ownable"
	tokenmanager "github.com/ethereum/go-ethereum/cmd/token-bind-tool/tokenmanger"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	tokenManager   = common.HexToAddress("0x0000000000000000000000000000000000001008")
	ledgerBasePath = accounts.DerivationPath{0x80000000 + 44, 0x80000000 + 60, 0x80000000 + 0, 0, 0}
)

type Config interface {
	Validate() error
}

type BindConfig struct {
	ContractData  string `json:"contract_data"`
	Symbol        string `json:"symbol"`
	BEP2Symbol    string `json:"bep2_symbol"`
	LedgerAccount string `json:"ledger_account"`
}

func (bindConfig *BindConfig) Validate() error {
	_, err := hex.DecodeString(bindConfig.ContractData)
	if err != nil {
		return fmt.Errorf("invalid contract byte code: %s", err.Error())
	}
	if len(bindConfig.BEP2Symbol) == 0 {
		return fmt.Errorf("missing bep2 token symbol")
	}
	if !strings.HasPrefix(bindConfig.LedgerAccount, "0x") || len(bindConfig.LedgerAccount) != 42 {
		return fmt.Errorf("invalid ledger account, expect bsc address, like 0x4E656459ed25bF986Eea1196Bc1B00665401645d")
	}
	return nil
}

func printUsage() {
	fmt.Print("usage: ./token-bind-tool --network-type testnet --operation {initKey, deployContract, approveBindAndTransferOwnership, refundRestBNB, deploy_transferTokenAndOwnership_refund, approveBindFromLedger}\n")
}

func initFlags() {
	flag.String(utils.KeystorePath, utils.BindKeystore, "keystore path")
	flag.String(utils.NetworkType, utils.TestNet, "mainnet or testnet")
	flag.String(utils.ConfigPath, "", "config file path")
	flag.String(utils.Operation, "", "operation to perform, valid operation: initKey, deployContract, approveBindAndTransferOwnership, refundRestBNB, deploy_transferTokenAndOwnership_refund, approveBindFromLedger")
	flag.String(utils.BEP20ContractAddr, "", "bep20 contract address")
	flag.String(utils.LedgerAccount, "", "ledger account address")
	flag.Int64(utils.LedgerAccountNumber, 1, "ledger account number")
	flag.Int64(utils.LedgerAccountIndex, 0, "ledger account index")
	flag.String(utils.PeggyAmount, "", "peggy amount")
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		panic(err)
	}
}

func readConfigData(configPath string) (BindConfig, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return BindConfig{}, err
	}
	fileData, err := ioutil.ReadAll(file)
	if err != nil {
		return BindConfig{}, err
	}
	var config BindConfig
	err = json.Unmarshal(fileData, &config)
	if err != nil {
		return BindConfig{}, err
	}
	err = config.Validate()
	if err != nil {
		return BindConfig{}, err
	}
	return config, nil
}

func generateOrGetTempAccount(keystorePath string, chainId *big.Int) (*keystore.KeyStore, accounts.Account, error) {
	path, err := os.Getwd()
	if err != nil {
		return nil, accounts.Account{}, err
	}
	keyStore := keystore.NewKeyStore(keystorePath, keystore.StandardScryptN, keystore.StandardScryptP)
	if len(keyStore.Accounts()) == 0 {
		newAccount, err := keyStore.NewAccount(utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		err = keyStore.Unlock(newAccount, utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		fmt.Print(fmt.Sprintf("Create temp account: %s", newAccount.Address.String()))
		utils.PrintAddrExplorerUrl(", explorer url", newAccount.Address.String(), chainId)
		fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
		return keyStore, newAccount, nil
	} else if len(keyStore.Accounts()) == 1 {
		accountList := keyStore.Accounts()
		if len(accountList) != 1 {
			return nil, accounts.Account{}, err
		}
		account := accountList[0]
		err = keyStore.Unlock(account, utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		fmt.Print(fmt.Sprintf("Load temp account: %s", account.Address.String()))
		utils.PrintAddrExplorerUrl(", explorer url", account.Address.String(), chainId)
		fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
		return keyStore, account, nil
	} else {
		return nil, accounts.Account{}, fmt.Errorf("expect only one or zero keystore file in %s", filepath.Join(path, utils.BindKeystore))
	}
}

func openLedger(index uint32) (accounts.Wallet, accounts.Account, error) {
	ledgerHub, err := usbwallet.NewLedgerHub()
	if err != nil {
		return nil, accounts.Account{}, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}
	wallets := ledgerHub.Wallets()
	if len(wallets) == 0 {
		return nil, accounts.Account{}, fmt.Errorf("empty ledger wallet")
	}
	wallet := wallets[0]
	err = wallet.Close()
	if err != nil {
		fmt.Println(err.Error())
	}

	err = wallet.Open("")
	if err != nil {
		return nil, accounts.Account{}, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}

	walletStatus, err := wallet.Status()
	if err != nil {
		return nil, accounts.Account{}, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}
	fmt.Println(walletStatus)
	//fmt.Println(wallet.URL())

	ledgerPath := make(accounts.DerivationPath, len(ledgerBasePath))
	copy(ledgerPath, ledgerBasePath)
	ledgerPath[2] = ledgerPath[2] + index
	ledgerAccount, err := wallet.Derive(ledgerPath, true)
	if err != nil {
		return nil, accounts.Account{}, fmt.Errorf("failed to derive account from ledger: %v", err)
	}
	return wallet, ledgerAccount, nil
}

func main() {
	initFlags()

	networkType := viper.GetString(utils.NetworkType)
	configPath := viper.GetString(utils.ConfigPath)
	operation := viper.GetString(utils.Operation)
	if networkType != utils.TestNet && networkType != utils.Mainnet || operation == "" {
		printUsage()
		return
	}
	var rpcClient *rpc.Client
	var err error
	var chainId *big.Int
	if networkType == utils.Mainnet {
		rpcClient, err = rpc.DialContext(context.Background(), utils.MainnnetRPC)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		chainId = big.NewInt(utils.MainnetChainID)
	} else {
		rpcClient, err = rpc.DialContext(context.Background(), utils.TestnetRPC)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		chainId = big.NewInt(utils.TestnetChainID)
	}
	ethClient := ethclient.NewClient(rpcClient)
	keystorePath := viper.GetString(utils.KeystorePath)

	switch operation {
	case utils.InitKey:
		_, _, err := generateOrGetTempAccount(keystorePath, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		return
	case utils.DeployContract:
		configData, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		keyStore, tempAccount, err := generateOrGetTempAccount(keystorePath, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		_, err = DeployContractFromTempAccount(ethClient, keyStore, tempAccount, configData.ContractData, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
	case utils.ApproveBind:
		bep20ContractAddr := viper.GetString(utils.BEP20ContractAddr)
		if strings.HasPrefix(bep20ContractAddr, "0x") || len(bep20ContractAddr) == 42 {
			fmt.Println("Invalid bep20 contract address")
			return
		}
		configData, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		keyStore, tempAccount, err := generateOrGetTempAccount(keystorePath, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		ApproveBindAndTransferOwnershipAndRestBalanceBackToLedgerAccount(ethClient, keyStore, tempAccount, configData, common.HexToAddress(bep20ContractAddr), chainId)
	case utils.RefundRestBNB:
		ledgerAccountStr := viper.GetString(utils.LedgerAccount)
		if strings.HasPrefix(ledgerAccountStr, "0x") || len(ledgerAccountStr) == 42 {
			fmt.Println("Invalid refund address")
			return
		}
		keyStore, tempAccount, err := generateOrGetTempAccount(keystorePath, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		RefundRestBNB(ethClient, keyStore, tempAccount, common.HexToAddress(ledgerAccountStr), chainId)
	case utils.DeployTransferRefund:
		config, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		keyStore, tempAccount, err := generateOrGetTempAccount(keystorePath, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		contractAddr, err := DeployContractFromTempAccount(ethClient, keyStore, tempAccount, config.ContractData, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		TransferTokenAndOwnership(ethClient, keyStore, tempAccount, common.HexToAddress(config.LedgerAccount), contractAddr, chainId)
		RefundRestBNB(ethClient, keyStore, tempAccount, common.HexToAddress(config.LedgerAccount), chainId)
	case utils.ApproveBindFromLedger:
		ledgerAccountIndex := viper.GetInt32(utils.LedgerAccountIndex)
		peggyAmountStr := viper.GetString(utils.PeggyAmount)
		peggyAmount := new(big.Int)
		peggyAmount, ok := peggyAmount.SetString(peggyAmountStr, 10)
		if !ok {
			fmt.Println("invalid peggy amount")
			return
		}

		bep20ContractAddr := viper.GetString(utils.BEP20ContractAddr)
		if strings.HasPrefix(bep20ContractAddr, "0x") || len(bep20ContractAddr) == 42 {
			fmt.Println("Invalid bep20 contract address")
			return
		}
		config, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		ledgerWallet, ledgerAccount, err := openLedger(uint32(ledgerAccountIndex))
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		ApproveBind(ethClient, ledgerWallet, ledgerAccount, config.BEP2Symbol, common.HexToAddress(bep20ContractAddr), peggyAmount,  chainId)
	default:
		fmt.Println(fmt.Sprintf("unsupported operation: %s", operation))
	}
}

func DeployContractFromTempAccount(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, contractByteCodeStr string, chainId *big.Int) (common.Address, error) {
	fmt.Println(fmt.Sprintf("Deploy BEP20 contract from account %s", tempAccount.Address.String()))
	contractByteCode, err := hex.DecodeString(contractByteCodeStr)
	if err != nil {
		return common.Address{}, err
	}
	txHash, err := utils.DeployBEP20Contract(ethClient, keyStore, tempAccount, contractByteCode, chainId)
	if err != nil {
		return common.Address{}, err
	}
	utils.PrintTxExplorerUrl("Deploy BEP20 contract txHash", txHash.String(), chainId)
	utils.Sleep(10)

	txRecipient, err := ethClient.TransactionReceipt(context.Background(), txHash)
	if err != nil {
		return common.Address{}, err
	}
	contractAddr := txRecipient.ContractAddress
	fmt.Print(fmt.Sprintf("The deployed BEP20 contract address is %s", contractAddr.String()))
	utils.PrintAddrExplorerUrl(", explorer url", contractAddr.String(), chainId)
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
	return contractAddr, nil
}

func ApproveBindAndTransferOwnershipAndRestBalanceBackToLedgerAccount(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, configData BindConfig, bep20ContractAddr common.Address, chainId *big.Int) {
	bep20Instance, err := bep20.NewBep20(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	totalSupply, err := bep20Instance.TotalSupply(utils.GetCallOpts())
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Total Supply %s", totalSupply.String()))

	fmt.Println(fmt.Sprintf("Approve %s:%s to TokenManager from %s", totalSupply.String(), configData.Symbol, tempAccount.Address.String()))
	approveTxHash, err := bep20Instance.Approve(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), tokenManager, totalSupply)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Approve token to tokenManager txHash", approveTxHash.Hash().String(), chainId)

	utils.Sleep(20)

	tokenManagerInstance, _ := tokenmanager.NewTokenmanager(tokenManager, ethClient)
	approveBindTx, err := tokenManagerInstance.ApproveBind(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(1e16)), bep20ContractAddr, configData.BEP2Symbol)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("ApproveBind txHash", approveBindTx.Hash().String(), chainId)

	utils.Sleep(10)

	approveBindTxRecipient, err := ethClient.TransactionReceipt(context.Background(), approveBindTx.Hash())
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println("Track approveBind Tx status")
	if approveBindTxRecipient.Status != 1 {
		fmt.Println("Approve Bind Failed")
		rejectBindTx, err := tokenManagerInstance.RejectBind(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(1e16)), bep20ContractAddr, configData.BEP2Symbol)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		utils.PrintTxExplorerUrl("RejectBind txHash", rejectBindTx.Hash().String(), chainId)
		utils.Sleep(10)
		fmt.Println("Track rejectBind Tx status")
		rejectBindTxRecipient, err := ethClient.TransactionReceipt(context.Background(), rejectBindTx.Hash())
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		fmt.Println(fmt.Sprintf("reject bind tx recipient status %d", rejectBindTxRecipient.Status))
		return
	}

	utils.Sleep(10)
	ownershipInstance, err := ownable.NewOwnable(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Transfer ownership %s %s to ledger account %s", totalSupply.String(), configData.Symbol, tempAccount.Address.String()))
	transferOwnerShipTxHash, err := ownershipInstance.TransferOwnership(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), common.HexToAddress(configData.LedgerAccount))
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Transfer ownership txHash", transferOwnerShipTxHash.Hash().String(), chainId)
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
}

func ApproveBind(ethClient *ethclient.Client, ledgerWallet accounts.Wallet, ledgerAccount accounts.Account, bep2Symbol string, bep20ContractAddr common.Address, peggyAmount *big.Int, chainId *big.Int) {
	fmt.Println(fmt.Sprintf("Approve %s:%s to TokenManager from %s", peggyAmount.String(), tokenManager.String()))
	bep20ABI, _ := abi.JSON(strings.NewReader(bep20.Bep20ABI))
	approveTxData, err := bep20ABI.Pack("approve", tokenManager, peggyAmount)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	hexApproveTxData := hexutil.Bytes(approveTxData)
	approveTx, err := utils.SendTransactionFromLedger(ethClient, ledgerWallet, ledgerAccount, bep20ContractAddr, big.NewInt(0), &hexApproveTxData, chainId)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Approve token to tokenManager txHash", approveTx.Hash().String(), chainId)

	utils.Sleep(20)

	tokenManagerABI, _ := abi.JSON(strings.NewReader(utils.TokenManagerABI))
	approveBindTxData, err := tokenManagerABI.Pack("approveBind", bep20ContractAddr, bep2Symbol)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	hexApproveBindTxData := hexutil.Bytes(approveBindTxData)
	approveBindTx, err := utils.SendTransactionFromLedger(ethClient, ledgerWallet, ledgerAccount, tokenManager, big.NewInt(0), &hexApproveBindTxData, chainId)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("ApproveBind txHash", approveBindTx.Hash().String(), chainId)
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
}

func TransferTokenAndOwnership(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, tokenOwner common.Address, bep20ContractAddr common.Address, chainId *big.Int) {
	bep20Instance, err := bep20.NewBep20(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	totalSupply, err := bep20Instance.TotalSupply(utils.GetCallOpts())
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Total Supply %s", totalSupply.String()))

	fmt.Println(fmt.Sprintf("Transfer %s token to %s", totalSupply.String(), tokenOwner.String()))
	transferTxHash, err := bep20Instance.Transfer(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), tokenOwner, totalSupply)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Transfer token txHash", transferTxHash.Hash().String(), chainId)

	utils.Sleep(10)

	ownershipInstance, err := ownable.NewOwnable(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Transfer ownership to %s", tokenOwner.String()))
	transferOwnerShipTxHash, err := ownershipInstance.TransferOwnership(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), tokenOwner)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Transfer ownership txHash", transferOwnerShipTxHash.Hash().String(), chainId)
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
}

func RefundRestBNB(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, refundAddr common.Address, chainId *big.Int) {
	utils.Sleep(10)
	txHash, err := utils.SendAllRestBNB(ethClient, keyStore, tempAccount, refundAddr, chainId)
	if err != nil {
		fmt.Println(err.Error())
	}
	utils.PrintTxExplorerUrl("Refund txHash", txHash.String(), chainId)
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------")
}
