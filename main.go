package main

import (
        "context"
        "fmt"
        "log"
        "time"

        "github.com/docker/docker/api/types/container"
        "github.com/docker/docker/client"
        "github.com/prometheus/client_golang/api"
        v1 "github.com/prometheus/client_golang/api/prometheus/v1"
        "github.com/prometheus/common/model"
)

const (
        prometheusURL   = "http://localhost:9090"
        thresholdCPU    = 80.0
        thresholdMemUp  = 75.0
        thresholdMemDown= 40.0
        scaleUpFactor   = 1.2
        scaleDownFactor = 0.8
        containerName   = "nginx-monitored"
)

/*
Essa função (fetchPrometheusMetrics) se conecta ao Prometheus para executar uma  
consulta e obter uma métrica. Faz isso criando o cliente Prometheus utilizando a URL configurada, 
executa a consulta ao endpoint com timeout de 10 seg, e retorna o valor obtido (ou erro)
*/

func fetchPrometheusMetrics(query string) (float64, error) {
        client, err := api.NewClient(api.Config{Address: prometheusURL})
        if err != nil {
                return 0, fmt.Errorf("erro ao criar cliente do Prometheus: %v", err)
        }

        v1api := v1.NewAPI(client)
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        result, _, err := v1api.Query(ctx, query, time.Now())
        if err != nil {
                return 0, fmt.Errorf("erro ao consultar o Prometheus: %v", err)
        }

        vectorVal, ok := result.(model.Vector)
        if !ok || len(vectorVal) == 0 {
                return 0, nil
        }

        return float64(vectorVal[0].Value), nil
}

/*
Esta função consulta o uso da CPU do contêiner especificado, que no caso é o nginx-monitored (containerName).
Para isso é montada a query PromQL para obter a taxa de uso do contêiner nos últimos 15 seg, utilizando a
função fetchPrometheusMetrics mostrada anteriormente. Depois calcula a porcentagem de uso em relação ao limite
atual e retorna o valor
*/

func fetchCPUMetrics(containerName string, currentCPULimit float64) (float64, error) {
        query := fmt.Sprintf(`rate(container_cpu_usage_seconds_total{name="%s"}[15s])`, containerName)
        cpuUsage, err := fetchPrometheusMetrics(query)
        if err != nil {
                return 0, err
        }
        return (cpuUsage / currentCPULimit) * 100, nil
}

/*
Esta função consulta a quantidade de memória utilizada pelo contêiner, da mesma forma que é feito o fetchCPUMetrics
*/

func fetchMemoryMetrics() (float64, error) {
        query := fmt.Sprintf(`container_memory_working_set_bytes{name="%s"}`, containerName)
        memUsage, err := fetchPrometheusMetrics(query)
        if err != nil {
                return 0, err
        }
        return memUsage / (1024 * 1024), nil // Converte p/ MB
}

/*
Atualiza os limites de CPU e memória do contêiner, para isso cria um cliente Docker para interagir
com a API, define os novos valores de CPU e memória, convertando a CPU para NanoCPU e a memória
para bytes, depois chama "ContainerUpdate" pra aplicar as novas configs do contêiner e registra
os novos limites aplicados no log
*/

func updateContainerResources(containerID string, newCPULimit float64, newMemoryLimit int64) error {
        cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
        if err != nil {
                return fmt.Errorf("erro ao criar cliente Docker: %v", err)
        }

        resources := container.Resources{
                NanoCPUs:   int64(newCPULimit * 1e9),
                Memory:     newMemoryLimit,
                MemorySwap: newMemoryLimit * 2,
        }

        updateConfig := container.UpdateConfig{Resources: resources}

        _, err = cli.ContainerUpdate(context.Background(), containerID, updateConfig)
        if err != nil {
                return fmt.Errorf("erro ao atualizar os recursos do contêiner: %v", err)
        }

        log.Printf("Novo limite aplicado: CPU = %.2f cores, Memória = %.2f MB", newCPULimit, float64(newMemoryLimit)/(1024*1024))
        return nil
}

/*
Faz as verificações para ajustar dinamicamente os recursos do conteiner com base na utilização atual
e caso necessário chama a função updateContainerResources para atualizar os recursos. Como a máquina 
*/

func adjustContainer(containerID string, cpuUsage float64, memUsage float64, currentCPULimit *float64, currentMemoryLimit *int64, initialMemoryLimit int64) error {
    newCPULimit := *currentCPULimit
    newMemoryLimit := *currentMemoryLimit

    if cpuUsage > thresholdCPU {
        newCPULimit *= scaleUpFactor
        // Como o host tem 4 vCPU, setei o limite como 4 para o código não tentar ultrapassar os limites
        if newCPULimit > 4.0 {
            newCPULimit = 4.0
        }
        log.Printf("🔼 Aumentando CPU para %.2f cores", newCPULimit)
    } else if cpuUsage < thresholdCPU*0.5 {
        newCPULimit *= scaleDownFactor
        // Valor definido no docker-compose, o quanto o container "nginx-monitored" deve ter de CPU
        if newCPULimit < 1.0 {
            newCPULimit = 1.0
        }
        log.Printf("🔽 Reduzindo CPU para %.2f cores", newCPULimit)
    }

    usagePercentage := (memUsage * 100) / float64(*currentMemoryLimit/(1024*1024))

    if usagePercentage > thresholdMemUp {
        newMemoryLimit = int64(float64(*currentMemoryLimit) * scaleUpFactor)
        // Host tem 8gb de ram
        if newMemoryLimit > (8 * 1024 * 1024 * 1024) {
            newMemoryLimit = 8 * 1024 * 1024 * 1024
        }
        log.Printf("🔼 Aumentando memória para %.2f MB", float64(newMemoryLimit)/(1024*1024))
    } else if usagePercentage < thresholdMemDown {
        newMemoryLimit = int64(float64(*currentMemoryLimit) * scaleDownFactor)
        // initialmemory = 512mb, definido na main (ainda vou arrumar para a CPU)
        if newMemoryLimit < initialMemoryLimit {
            newMemoryLimit = initialMemoryLimit
        }
        log.Printf("🔽 Reduzindo memória para %.2f MB", float64(newMemoryLimit)/(1024*1024))
    }

    if newCPULimit != *currentCPULimit || newMemoryLimit != *currentMemoryLimit {
        err := updateContainerResources(containerID, newCPULimit, newMemoryLimit)
        if err != nil {
            return err
        }
        *currentCPULimit = newCPULimit
        *currentMemoryLimit = newMemoryLimit
    }

    log.Printf("📊 Uso atual - CPU: %.2f%%, Memória: %.2f MB (%.2f%% do limite)", cpuUsage, memUsage, usagePercentage)
    return nil
}

func main() {
    currentCPULimit := 1.0
    initialMemoryLimit := int64(512 * 1024 * 1024) // Armazena o valor inicial (512MB)
    currentMemoryLimit := initialMemoryLimit

    for {
        // Separar melhor os logs
        log.Println("=========================================")

        cpuUsage, err := fetchCPUMetrics(containerName, currentCPULimit)
        if err != nil {
            log.Printf("❌ Erro ao buscar métricas de CPU: %v", err)
            continue
        }

        memUsage, err := fetchMemoryMetrics()
        if err != nil {
            log.Printf("❌ Erro ao buscar métricas de memória: %v", err)
            continue
        }

        err = adjustContainer(containerName, cpuUsage, memUsage, &currentCPULimit, &currentMemoryLimit, initialMemoryLimit)
        if err != nil {
            log.Printf("❌ Erro ao ajustar contêiner: %v", err)
        }

        time.Sleep(5 * time.Second)
    }
}
