package deploy

import (
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Layr-Labs/eigenda/core"
)

const (
	churnerImage   = "ghcr.io/layr-labs/eigenda/churner:local"
	disImage       = "ghcr.io/layr-labs/eigenda/disperser:local"
	encoderImage   = "ghcr.io/layr-labs/eigenda/encoder:local"
	batcherImage   = "ghcr.io/layr-labs/eigenda/batcher:local"
	nodeImage      = "ghcr.io/layr-labs/eigenda/node:local"
	retrieverImage = "ghcr.io/layr-labs/eigenda/retriever:local"
)

func (env *Config) getKeyString(name string) string {
	key, _ := env.getKey(name)
	keyInt, ok := new(big.Int).SetString(key, 0)
	if !ok {
		log.Panicf("Error: could not parse key %s", key)
	}
	return keyInt.String()
}

func (env *Config) generateEigenDADeployConfig() EigenDADeployConfig {

	operators := make([]string, 0)
	stakers := make([]string, 0)
	maxOperatorCount := env.Services.Counts.NumMaxOperatorCount

	total := float32(0)
	stakes := [][]string{make([]string, len(env.Services.Stakes.Distribution))}

	for _, stake := range env.Services.Stakes.Distribution {
		total += stake
	}

	for ind, stake := range env.Services.Stakes.Distribution {
		stakes[0][ind] = strconv.FormatFloat(float64(stake/total*env.Services.Stakes.Total), 'f', 0, 32)
	}

	for i := 0; i < len(env.Services.Stakes.Distribution); i++ {
		stakerName := fmt.Sprintf("staker%d", i)
		operatorName := fmt.Sprintf("opr%d", i)

		stakers = append(stakers, env.getKeyString(stakerName))
		operators = append(operators, env.getKeyString(operatorName))
	}

	config := EigenDADeployConfig{
		UseDefaults:         true,
		NumStrategies:       1,
		MaxOperatorCount:    maxOperatorCount,
		StakerPrivateKeys:   stakers,
		StakerTokenAmounts:  stakes,
		OperatorPrivateKeys: operators,
	}

	return config

}

func (env *Config) deployEigenDAContracts() {
	log.Print("Deploy the EigenDA and EigenLayer contracts")

	// get deployer
	deployer, ok := env.GetDeployer(env.EigenDA.Deployer)
	if !ok {
		log.Panicf("Deployer improperly configured")
	}

	changeDirectory(filepath.Join(env.rootPath, "contracts"))

	eigendaDeployConfig := env.generateEigenDADeployConfig()
	data, err := json.Marshal(&eigendaDeployConfig)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}
	writeFile("script/eigenda_deploy_config.json", data)

	execForgeScript("script/SetUpEigenDA.s.sol:SetupEigenDA", env.Pks.EcdsaMap[deployer.Name].PrivateKey, deployer, nil)

	//add relevant addresses to path
	data = readFile("script/output/eigenda_deploy_output.json")
	err = json.Unmarshal(data, &env.EigenDA)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}
	blobHeader := &core.BlobHeader{
		QuorumInfos: []*core.BlobQuorumInfo{
			{
				SecurityParam: core.SecurityParam{
					QuorumID:           0,
					AdversaryThreshold: 80,
					QuorumThreshold:    100,
				},
				QuantizationFactor: 1,
			},
		},
	}
	hash, err := blobHeader.GetQuorumBlobParamsHash()
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}
	hashStr := fmt.Sprintf("%x", hash)
	execForgeScript("script/MockRollupDeployer.s.sol:MockRollupDeployer", env.Pks.EcdsaMap[deployer.Name].PrivateKey, deployer, []string{"--sig", "run(address,bytes32,uint256)", env.EigenDA.ServiceManager, hashStr, big.NewInt(1e18).String()})

	//add rollup address to path
	data = readFile("script/output/mock_rollup_deploy_output.json")
	var rollupAddr struct{ MockRollup string }
	err = json.Unmarshal(data, &rollupAddr)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}

	env.MockRollup = rollupAddr.MockRollup
}

// Deploys a EigenDA experiment
func (env *Config) DeployExperiment() {

	defer env.SaveTestConfig()

	log.Print("Deploying experiment...")

	// Log to file
	f, err := os.OpenFile(filepath.Join(env.Path, "deploy.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Panicf("error opening file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	// Create a new experiment and deploy the contracts

	err = env.loadPrivateKeys()
	if err != nil {
		log.Panicf("could not load private keys: %v", err)
	}

	if env.EigenDA.Deployer != "" && !env.IsEigenDADeployed() {
		fmt.Println("Deploying EigenDA")
		env.deployEigenDAContracts()
	}

	if deployer, ok := env.GetDeployer(env.EigenDA.Deployer); ok && deployer.DeploySubgraphs {
		startBlock := GetLatestBlockNumber(env.Deployers[0].RPC)
		env.deploySubgraphs(startBlock)
	}

	fmt.Println("Generating variables")
	env.GenerateAllVariables()

	fmt.Println("Test environment has succesfully deployed!")
}

// TODO: Supply the test path to the runner utility
func (env *Config) StartBinaries() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"start-detached"}, []string{})

	if err != nil {
		log.Panicf("Failed to start binaries. Err: %s", err)
	}
}

// TODO: Supply the test path to the runner utility
func (env *Config) StopBinaries() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"stop"}, []string{})
	if err != nil {
		log.Panicf("Failed to stop binaries. Err: %s", err)
	}
}

func (env *Config) StartAnvil() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"start-anvil"}, []string{})
	if err != nil {
		log.Panicf("Failed to start anvil. Err: %s", err)
	}
}

func (env *Config) StopAnvil() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"stop-anvil"}, []string{})
	if err != nil {
		log.Panicf("Failed to stop anvil. Err: %s", err)
	}
}