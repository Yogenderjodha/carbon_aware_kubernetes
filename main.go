package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// ---------- Config ----------

type CountryConfig struct {
        Name           string // "Denmark"
        RegionLabel    string // "denmark" (node label value)
        Zone           string // ElectricityMaps zone code, e.g., "DK", "FR", "LV"
        TokenEnv       string // e.g. "DENMARK_TOKEN"
        DeploymentName string // e.g. "nginx-denmark"
}

type CarbonData struct {
        Zone            string  `json:"zone"`
        CarbonIntensity float64 `json:"carbonIntensity"`
        Datetime        string  `json:"datetime"`
        UpdatedAt       string  `json:"updatedAt"`
        IsEstimated     bool    `json:"isEstimated"`
}

type NodeMetrics struct {
        NodeName        string
        CPUUsagePercent float64
        CarbonIntensity float64
        Region          string // from node label
        Country         string // pretty name (e.g., Denmark)
}

type cacheEntry struct {
        val     float64
        expires time.Time
}

type Autoscaler struct {
        log               *zap.Logger
        consolePretty     bool
        useColor          bool

        k8sClient         kubernetes.Interface
        metricsClient     metricsv1beta1.Interface
        namespace         string
        countriesByRegion map[string]CountryConfig // key: region label value
        countriesList     []CountryConfig          // stable iteration order
        carbonCache       map[string]cacheEntry
        cacheTTL          time.Duration

        loopPeriod         time.Duration
        scaleUpThreshold   float64
        scaleDownThreshold float64
        holdCycles         int
        cooldown           time.Duration
        minReplicas        int32
        maxReplicas        int32

        // state
        cycleCount    int
        consecHighCPU int
        consecLowCPU  int
        lastScaleAt   time.Time
}

// ---------- env helpers ----------

func mustEnvDuration(key string, def time.Duration) time.Duration {
        if v := os.Getenv(key); v != "" {
                if d, err := time.ParseDuration(v); err == nil {
                        return d
                }
        }
        return def
}
func mustEnvFloat(key string, def float64) float64 {
        if v := os.Getenv(key); v != "" {
                if f, err := strconv.ParseFloat(v, 64); err == nil {
                        return f
                }
        }
        return def
}
func mustEnvInt(key string, def int) int {
        if v := os.Getenv(key); v != "" {
                if n, err := strconv.Atoi(v); err == nil {
                        return n
                }
        }
        return def
}
func mustEnvInt32(key string, def int32) int32 {
        if v := os.Getenv(key); v != "" {
                if n, err := strconv.Atoi(v); err == nil {
                        return int32(n)
                }
        }
        return def
}
func envOrDefault(key, def string) string {
        if v := os.Getenv(key); v != "" {
                return v
        }
        return def
}

// ---------- pretty helpers ----------

const (
        cReset  = "\033[0m"
        cGreen  = "\033[32m"
        cRed    = "\033[31m"
        cYellow = "\033[33m"
        cCyan   = "\033[36m"
        cBold   = "\033[1m"
)

func flag(region string) string {
        switch strings.ToLower(region) {
        case "denmark":
                return "🇩🇰"
        case "france":
                return "🇫🇷"
        case "latvia":
                return "🇱🇻"
        default:
                return "🏳️"
        }
}

func colorize(s, col string, enabled bool) string {
        if !enabled {
                return s
        }
        return col + s + cReset
}

func title(s string) string {
        if s == "" {
                return s
        }
        return strings.ToUpper(s[:1]) + s[1:]
}

// ---------- logger ----------

func newLogger() (*zap.Logger, bool, bool) {
        format := strings.ToLower(envOrDefault("LOG_FORMAT", "json")) // "json" or "console"
        useColor := strings.ToLower(envOrDefault("USE_COLOR", "true")) != "false"

        if format == "console" {
                encCfg := zapcore.EncoderConfig{
                        MessageKey: "msg",
                        LevelKey:   "level",
                        TimeKey:    "ts",
                        CallerKey:  "caller",
                        EncodeTime: zapcore.TimeEncoderOfLayout("15:04:05"),
                        EncodeLevel: zapcore.CapitalColorLevelEncoder,
                        EncodeCaller: zapcore.ShortCallerEncoder,
                }
                core := zapcore.NewCore(
                        zapcore.NewConsoleEncoder(encCfg),
                        zapcore.AddSync(os.Stdout),
                        zapcore.InfoLevel,
                )
                return zap.New(core, zap.AddCaller()), true, useColor
        }

        // default: production JSON
        l, _ := zap.NewProduction()
        return l, false, useColor
}

func main() {
        logger, consolePretty, useColor := newLogger()
        defer logger.Sync()
        log := logger.Named("carbon-autoscaler")

        log.Info("Starting Carbon-Aware Autoscaler")

        config, err := rest.InClusterConfig()
        if err != nil {
                log.Fatal("Failed to create in-cluster config", zap.Error(err))
        }

        k8sClient, err := kubernetes.NewForConfig(config)
        if err != nil {
                log.Fatal("Failed to create Kubernetes client", zap.Error(err))
        }

        metricsClient, err := metricsv1beta1.NewForConfig(config)
        if err != nil {
                log.Warn("Metrics client unavailable; falling back to simulated CPU", zap.Error(err))
                metricsClient = nil
        }

        // Countries: map node label "region" -> config
        countries := []CountryConfig{
                {
                        Name:           "Denmark",
                        RegionLabel:    "denmark",
                        Zone:           envOrDefault("DENMARK_ZONE", "DK"),
                        TokenEnv:       "DENMARK_TOKEN",
                        DeploymentName: envOrDefault("DENMARK_DEPLOYMENT", "nginx-denmark"),
                },
                {
                        Name:           "France",
                        RegionLabel:    "france",
                        Zone:           envOrDefault("FRANCE_ZONE", "FR"),
                        TokenEnv:       "FRANCE_TOKEN",
                        DeploymentName: envOrDefault("FRANCE_DEPLOYMENT", "nginx-france"),
                },
                {
                        Name:           "Latvia",
                        RegionLabel:    "latvia",
                        Zone:           envOrDefault("LATVIA_ZONE", "LV"),
                        TokenEnv:       "LATVIA_TOKEN",
                        DeploymentName: envOrDefault("LATVIA_DEPLOYMENT", "nginx-latvia"),
                },
        }
        countriesByRegion := map[string]CountryConfig{}
        for _, c := range countries {
                countriesByRegion[c.RegionLabel] = c
        }

        autoscaler := &Autoscaler{
                log:               log,
                consolePretty:     consolePretty,
                useColor:          useColor,
                k8sClient:         k8sClient,
                metricsClient:     metricsClient,
                namespace:         envOrDefault("NAMESPACE", "default"),
                countriesByRegion: countriesByRegion,
                countriesList:     countries,
                carbonCache:       make(map[string]cacheEntry),
                cacheTTL:          mustEnvDuration("CARBON_CACHE_TTL", 5*time.Minute),

                loopPeriod:         mustEnvDuration("LOOP_PERIOD", 30*time.Second),
                scaleUpThreshold:   mustEnvFloat("SCALE_UP_THRESHOLD", 45.0),
                scaleDownThreshold: mustEnvFloat("SCALE_DOWN_THRESHOLD", 25.0),
                holdCycles:         mustEnvInt("HOLD_CYCLES", 3),
                cooldown:           mustEnvDuration("COOLDOWN", 2*time.Minute),
                minReplicas:        mustEnvInt32("MIN_REPLICAS", 0),
                maxReplicas:        mustEnvInt32("MAX_REPLICAS", 10),
        }

        tick := time.NewTicker(autoscaler.loopPeriod)
        defer tick.Stop()

        for {
                if err := autoscaler.runScalingCycle(); err != nil {
                        autoscaler.log.Warn("Scaling cycle error", zap.Error(err))
                }
                <-tick.C
        }
}

// ---------- Cycle ----------

func (a *Autoscaler) runScalingCycle() error {
        ctx := context.Background()
        a.cycleCount++

        nodes, err := a.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
        if err != nil {
                return fmt.Errorf("list nodes: %w", err)
        }

        var nodeMetrics []NodeMetrics

        for _, node := range nodes.Items {
                // skip control-plane/master
                if strings.Contains(node.Name, "control-plane") || strings.Contains(node.Name, "master") {
                        continue
                }
                // ready?
                ready := false
                for _, cond := range node.Status.Conditions {
                        if string(cond.Type) == "Ready" && string(cond.Status) == "True" {
                                ready = true
                                break
                        }
                }
                if !ready {
                        continue
                }
                region := strings.ToLower(node.Labels["region"])
                if region == "" {
                        a.log.Warn("Node missing region label; skipping", zap.String("node", node.Name))
                        continue
                }

                countryCfg, ok := a.countriesByRegion[region]
                if !ok {
                        a.log.Warn("Unknown region; no country mapping", zap.String("node", node.Name), zap.String("region", region))
                        continue
                }

                // CPU
                var cpuUsage float64
                if a.metricsClient != nil {
                        cpuUsage, err = a.getNodeCPUUsage(node.Name)
                        if err != nil {
                                a.log.Warn("Failed to get node CPU (using simulated)", zap.String("node", node.Name), zap.Error(err))
                                cpuUsage = a.getSimulatedCPU()
                        }
                } else {
                        cpuUsage = a.getSimulatedCPU()
                }

                // Carbon (with cache)
                carbon, err := a.getCarbonIntensity(countryCfg)
                if err != nil {
                        a.log.Warn("Carbon API failed; using mock", zap.String("region", region), zap.Error(err))
                        carbon = a.getMockCarbonIntensity(countryCfg)
                }

                nodeMetrics = append(nodeMetrics, NodeMetrics{
                        NodeName:        node.Name,
                        CPUUsagePercent: cpuUsage,
                        CarbonIntensity: carbon,
                        Region:          region,
                        Country:         countryCfg.Name,
                })
        }

        if len(nodeMetrics) == 0 {
                return fmt.Errorf("no valid worker nodes found")
        }

        // Ensure baseline: every deployment must have at least minReplicas
        if a.minReplicas > 0 {
                healed := a.ensureMinReplicas()
                if healed && a.consolePretty {
                        a.log.Info(colorize("🔧 Baseline healed to MIN_REPLICAS", cCyan, a.useColor),
                                zap.Int32("minReplicas", a.minReplicas))
                } else if healed {
                        a.log.Info("Baseline healed to MIN_REPLICAS", zap.Int32("minReplicas", a.minReplicas))
                }
        }

        // log node summary (pretty + fields)
        for _, m := range nodeMetrics {
                pretty := fmt.Sprintf("%s %s (%s)  |  %.0f g/kWh  |  CPU %.1f%%",
                        flag(m.Region), strings.ToLower(m.Region), m.NodeName, m.CarbonIntensity, m.CPUUsagePercent)
                if a.consolePretty {
                        a.log.Info(pretty,
                                zap.String("node", m.NodeName),
                                zap.String("region", m.Region),
                                zap.Float64("carbon_g_per_kwh", m.CarbonIntensity),
                                zap.Float64("cpu_pct", m.CPUUsagePercent),
                        )
                } else {
                        a.log.Info("Node status",
                                zap.String("pretty", pretty),
                                zap.String("node", m.NodeName),
                                zap.String("region", m.Region),
                                zap.Float64("carbon_g_per_kwh", m.CarbonIntensity),
                                zap.Float64("cpu_pct", m.CPUUsagePercent),
                        )
                }
        }

        return a.makeScalingDecisions(nodeMetrics)
}

// ---------- Metrics helpers ----------

func (a *Autoscaler) getNodeCPUUsage(nodeName string) (float64, error) {
        ctx := context.Background()
        nm, err := a.metricsClient.MetricsV1beta1().NodeMetricses().Get(ctx, nodeName, metav1.GetOptions{})
        if err != nil {
                return 0, err
        }
        node, err := a.k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
        if err != nil {
                return 0, err
        }
        capacity := node.Status.Capacity["cpu"]
        usage := nm.Usage["cpu"]
        capMillis := capacity.MilliValue()
        useMillis := usage.MilliValue()
        if capMillis == 0 {
                return 0, nil
        }
        return float64(useMillis) / float64(capMillis) * 100.0, nil
}

func (a *Autoscaler) getSimulatedCPU() float64 {
        // gentle oscillation 25–60%
        base := 25.0
        span := 35.0
        return base + float64((a.cycleCount%10))*span/10.0
}

// ---------- Carbon helpers (with cache) ----------

func (a *Autoscaler) getCarbonIntensity(country CountryConfig) (float64, error) {
        // cache key by region/zone
        key := country.RegionLabel
        if ent, ok := a.carbonCache[key]; ok && time.Now().Before(ent.expires) {
                return ent.val, nil
        }

        token := os.Getenv(country.TokenEnv)
        if len(token) < 8 {
                // no/short token -> mock
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("missing or short token for %s", country.Name)
        }

        url := fmt.Sprintf("https://api.electricitymaps.com/v3/carbon-intensity/latest?zone=%s", country.Zone)
        client := &http.Client{Timeout: 8 * time.Second}
        req, _ := http.NewRequest("GET", url, nil)
        req.Header.Set("auth-token", token)

        resp, err := client.Do(req)
        if err != nil {
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("carbon http error: %w", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("carbon http status: %d", resp.StatusCode)
        }

        body, err := io.ReadAll(resp.Body)
        if err != nil {
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("carbon read error: %w", err)
        }

        var cd CarbonData
        if err := json.Unmarshal(body, &cd); err != nil {
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("carbon unmarshal error: %w", err)
        }
        if cd.CarbonIntensity <= 0 {
                val := a.getMockCarbonIntensity(country)
                a.carbonCache[key] = cacheEntry{val: val, expires: time.Now().Add(a.cacheTTL)}
                return val, fmt.Errorf("invalid carbon value")
        }

        a.carbonCache[key] = cacheEntry{val: cd.CarbonIntensity, expires: time.Now().Add(a.cacheTTL)}
        return cd.CarbonIntensity, nil
}

func (a *Autoscaler) getMockCarbonIntensity(country CountryConfig) float64 {
        base := map[string]float64{
                "denmark": 120,
                "france":   80,
                "latvia":  200,
        }[country.RegionLabel]
        // small pseudo-variation
        variation := float64((a.cycleCount*7)%31) - 15 // [-15,+15]
        return base + variation
}

// ---------- Decision & scaling ----------

func (a *Autoscaler) makeScalingDecisions(nodeMetrics []NodeMetrics) error {
        // avg cluster CPU
        sum := 0.0
        for _, m := range nodeMetrics {
                sum += m.CPUUsagePercent
        }
        avgCPU := sum / float64(len(nodeMetrics))

        // rank cleanest → dirtiest
        sort.Slice(nodeMetrics, func(i, j int) bool {
                return nodeMetrics[i].CarbonIntensity < nodeMetrics[j].CarbonIntensity
        })

        // pretty ranking line
        rankParts := make([]string, len(nodeMetrics))
        for i, m := range nodeMetrics {
                rankParts[i] = fmt.Sprintf("%s(%.0f)", flag(m.Region), m.CarbonIntensity)
        }
        rankLine := fmt.Sprintf("🌱 Ranking:  %s   avg CPU %.1f%%", strings.Join(rankParts, " → "), avgCPU)
        if a.consolePretty {
                a.log.Info(rankLine)
        } else {
                a.log.Info("Ranking (cleanest→dirtiest)",
                        zap.String("ranking_pretty", strings.Join(rankParts, " → ")),
                        zap.Float64("avg_cpu_pct", avgCPU),
                        zap.String("cleanest", nodeMetrics[0].Region),
                        zap.Float64("cleanest_g_per_kwh", nodeMetrics[0].CarbonIntensity),
                        zap.String("dirtiest", nodeMetrics[len(nodeMetrics)-1].Region),
                        zap.Float64("dirtiest_g_per_kwh", nodeMetrics[len(nodeMetrics)-1].CarbonIntensity),
                )
        }

        now := time.Now()

        // cooldown check
        if !a.lastScaleAt.IsZero() && now.Sub(a.lastScaleAt) < a.cooldown {
                msg := fmt.Sprintf("⏳ Cooldown (%.0fs left)", (a.cooldown - now.Sub(a.lastScaleAt)).Seconds())
                if a.consolePretty {
                        a.log.Info(colorize(msg, cYellow, a.useColor))
                } else {
                        a.log.Info("In cooldown; skipping decisions",
                                zap.Duration("remaining", a.cooldown-now.Sub(a.lastScaleAt)))
                }
                return nil
        }

        // hold-cycle accumulation
        if avgCPU > a.scaleUpThreshold {
                a.consecHighCPU++
                a.consecLowCPU = 0
                if a.consolePretty {
                        a.log.Info(fmt.Sprintf("📈 High CPU  (%d/%d)", a.consecHighCPU, a.holdCycles))
                } else {
                        a.log.Info("High CPU detected", zap.Int("consecutive_high_cycles", a.consecHighCPU))
                }
                if a.consecHighCPU >= a.holdCycles {
                        target := nodeMetrics[0] // cleanest
                        a.consecHighCPU = 0
                        return a.scaleUp(target)
                }
                return nil
        }

        if avgCPU < a.scaleDownThreshold {
                a.consecLowCPU++
                a.consecHighCPU = 0
                if a.consolePretty {
                        a.log.Info(fmt.Sprintf("📉 Low CPU  (%d/%d)", a.consecLowCPU, a.holdCycles))
                } else {
                        a.log.Info("Low CPU detected", zap.Int("consecutive_low_cycles", a.consecLowCPU))
                }
                if a.consecLowCPU >= a.holdCycles {
                        a.consecLowCPU = 0
                        // choose dirtiest region that still has replicas > min
                        if target, ok := a.pickScaleDownTarget(nodeMetrics); ok {
                                return a.scaleDown(target)
                        }
                        a.log.Info("All regions at MIN_REPLICAS; no scale down performed",
                                zap.Int32("minReplicas", a.minReplicas))
                        return nil
                }
                return nil
        }

        // within band
        a.consecHighCPU = 0
        a.consecLowCPU = 0
        if a.consolePretty {
                a.log.Info("🟢 CPU within normal band; no scaling")
        } else {
                a.log.Info("CPU within normal band; no scaling decision")
        }
        return nil
}

// pickScaleDownTarget sorts regions by carbon DESC and returns the first with replicas > min.
func (a *Autoscaler) pickScaleDownTarget(nodeMetrics []NodeMetrics) (NodeMetrics, bool) {
        // copy and sort dirtiest → cleanest
        cpy := make([]NodeMetrics, len(nodeMetrics))
        copy(cpy, nodeMetrics)
        sort.Slice(cpy, func(i, j int) bool { return cpy[i].CarbonIntensity > cpy[j].CarbonIntensity })

        for _, m := range cpy {
                cfg, ok := a.countriesByRegion[m.Region]
                if !ok || cfg.DeploymentName == "" {
                        continue
                }
                rep, err := a.getReplicas(cfg.DeploymentName)
                if err != nil {
                        a.log.Warn("getReplicas failed", zap.String("deployment", cfg.DeploymentName), zap.Error(err))
                        continue
                }
                if rep > a.minReplicas {
                        return m, true
                }
        }
        return NodeMetrics{}, false
}

// ensureMinReplicas checks each configured deployment and bumps it to minReplicas if below.
// Returns true if it changed anything.
func (a *Autoscaler) ensureMinReplicas() bool {
        changed := false
        for _, cfg := range a.countriesList {
                if cfg.DeploymentName == "" {
                        continue
                }
                rep, err := a.getReplicas(cfg.DeploymentName)
                if err != nil {
                        a.log.Warn("ensureMinReplicas: getReplicas failed", zap.String("deployment", cfg.DeploymentName), zap.Error(err))
                        continue
                }
                if rep < a.minReplicas {
                        if err := a.setReplicas(cfg.DeploymentName, a.minReplicas); err != nil {
                                a.log.Warn("ensureMinReplicas: setReplicas failed", zap.String("deployment", cfg.DeploymentName), zap.Error(err))
                                continue
                        }
                        pretty := fmt.Sprintf("🔧 Healed  %s %s  %s  → %d",
                                flag(cfg.RegionLabel), cfg.RegionLabel, cfg.DeploymentName, a.minReplicas)
                        if a.consolePretty {
                                a.log.Info(colorize(pretty, cCyan, a.useColor))
                        } else {
                                a.log.Info("Healed to MIN_REPLICAS",
                                        zap.String("deployment", cfg.DeploymentName),
                                        zap.Int32("new_replicas", a.minReplicas))
                        }
                        changed = true
                        // no cooldown for healing
                }
        }
        return changed
}

// helpers for Scale subresource
func (a *Autoscaler) getReplicas(deploy string) (int32, error) {
        ctx := context.Background()
        scale, err := a.k8sClient.AppsV1().Deployments(a.namespace).GetScale(ctx, deploy, metav1.GetOptions{})
        if err != nil {
                return 0, err
        }
        return scale.Spec.Replicas, nil
}

func (a *Autoscaler) setReplicas(deploy string, replicas int32) error {
        ctx := context.Background()
        scale, err := a.k8sClient.AppsV1().Deployments(a.namespace).GetScale(ctx, deploy, metav1.GetOptions{})
        if err != nil {
                return err
        }
        scale.Spec.Replicas = replicas
        _, err = a.k8sClient.AppsV1().Deployments(a.namespace).UpdateScale(ctx, deploy, scale, metav1.UpdateOptions{})
        return err
}

func (a *Autoscaler) scaleUp(target NodeMetrics) error {
        cfg, ok := a.countriesByRegion[target.Region]
        if !ok || cfg.DeploymentName == "" {
                return fmt.Errorf("no deployment mapped for region=%s", target.Region)
        }
        rep, err := a.getReplicas(cfg.DeploymentName)
        if err != nil {
                a.log.Warn("GetScale failed; skipping scale up", zap.String("deployment", cfg.DeploymentName), zap.Error(err))
                return nil
        }

        newReplicas := rep + 1
        if newReplicas > a.maxReplicas {
                newReplicas = a.maxReplicas
        }
        if newReplicas == rep {
                msg := fmt.Sprintf("↔️  Max replicas reached  %s %s  %s  = %d",
                        flag(target.Region), target.Region, cfg.DeploymentName, rep)
                if a.consolePretty {
                        a.log.Info(colorize(msg, cYellow, a.useColor))
                } else {
                        a.log.Info("Scale up capped by maxReplicas",
                                zap.String("deployment", cfg.DeploymentName),
                                zap.Int32("replicas", rep),
                                zap.Int32("maxReplicas", a.maxReplicas))
                }
                return nil
        }

        if err := a.setReplicas(cfg.DeploymentName, newReplicas); err != nil {
                return fmt.Errorf("update scale up: %w", err)
        }
        a.lastScaleAt = time.Now()

        pretty := fmt.Sprintf("🟢 ↑ Scale Up  %s %s  %s  %d→%d  |  %.0f g/kWh  |  CPU %.1f%%",
                flag(target.Region), target.Region, cfg.DeploymentName, rep, newReplicas, target.CarbonIntensity, target.CPUUsagePercent)
        if a.consolePretty {
                a.log.Info(colorize(pretty, cGreen, a.useColor))
        } else {
                a.log.Info("Scaled up",
                        zap.String("deployment", cfg.DeploymentName),
                        zap.Int32("new_replicas", newReplicas),
                        zap.String("region", target.Region),
                        zap.Float64("carbon_g_per_kwh", target.CarbonIntensity),
                        zap.Float64("cpu_pct", target.CPUUsagePercent),
                )
        }
        return nil
}

func (a *Autoscaler) scaleDown(target NodeMetrics) error {
        cfg, ok := a.countriesByRegion[target.Region]
        if !ok || cfg.DeploymentName == "" {
                return fmt.Errorf("no deployment mapped for region=%s", target.Region)
        }
        rep, err := a.getReplicas(cfg.DeploymentName)
        if err != nil {
                a.log.Warn("GetScale failed; skipping scale down", zap.String("deployment", cfg.DeploymentName), zap.Error(err))
                return nil
        }

        newReplicas := rep - 1
        if newReplicas < a.minReplicas {
                newReplicas = a.minReplicas
        }
        if newReplicas == rep {
                msg := fmt.Sprintf("↔️  Min replicas reached  %s %s  %s  = %d",
                        flag(target.Region), target.Region, cfg.DeploymentName, rep)
                if a.consolePretty {
                        a.log.Info(colorize(msg, cYellow, a.useColor))
                } else {
                        a.log.Info("Scale down capped by minReplicas",
                                zap.String("deployment", cfg.DeploymentName),
                                zap.Int32("replicas", rep),
                                zap.Int32("minReplicas", a.minReplicas))
                }
                return nil
        }

        if err := a.setReplicas(cfg.DeploymentName, newReplicas); err != nil {
                return fmt.Errorf("update scale down: %w", err)
        }
        a.lastScaleAt = time.Now()

        pretty := fmt.Sprintf("🔴 ↓ Scale Down  %s %s  %s  %d→%d  |  %.0f g/kWh  |  CPU %.1f%%",
                flag(target.Region), target.Region, cfg.DeploymentName, rep, newReplicas, target.CarbonIntensity, target.CPUUsagePercent)
        if a.consolePretty {
                a.log.Info(colorize(pretty, cRed, a.useColor))
        } else {
                a.log.Info("Scaled down",
                        zap.String("deployment", cfg.DeploymentName),
                        zap.Int32("new_replicas", newReplicas),
                        zap.String("region", target.Region),
                        zap.Float64("carbon_g_per_kwh", target.CarbonIntensity),
                        zap.Float64("cpu_pct", target.CPUUsagePercent),
                )
        }
        return nil
}
