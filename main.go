package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/mcuadros/go-defaults"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/yalp/jsonpath"

	"github.com/spf13/pflag"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	lastPush = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "last_push_timestamp_seconds",
			Help: "Unix timestamp of the last received metrics push in seconds.",
		},
	)

	payloadTypeJson = "json"
	configFileName  = "mqtt_exporter"
	configFileExt   = "json"

	configuration = &Configuration{}
	config        = ExporterConfiguration{}
	collector     = &mqttCollector{}

	reCache = make(map[string]FilterCache)
)

type FilterCache struct {
	fre *regexp.Regexp
}

type ExporterConfig struct {
	ListeningAddress  string `mapstructure:"listeningAddress" default:":9393"`
	MetricsPath       string `mapstructure:"metricsPath" default:"/metrics"`
	ConfigurationFile string `mapstructure:"configurationFile"`
}

type ExporterMqttConfig struct {
	Broker   string `mapstructure:"broker" default:"tcp://127.0.0.1:1883"`
	ClientId string `mapstructure:"clientId" default:"mqtt_exporter_client"`
	Qos      byte   `mapstructure:"qos" default:"0"`
}

type ExporterConfiguration struct {
	Config ExporterConfig     `mapstructure:"config"`
	Mqtt   ExporterMqttConfig `mapstructure:"mqtt"`
}

type Entity struct {
	Name        string `json:"group"`
	LastUpdated string `json:"last_updated"`
}

type FiltersEntry struct {
	Filter string            `json:"filter"`
	Labels []string          `json:"labels"`
	Values map[string]string `json:"values"`
	Group  string            `json:"group"`
	Name   string            `json:"name"`
}

type Configuration struct {
	Filters     map[string]FiltersEntry `json:"filters"`
	Prefix      string                  `json:"prefix"`
	PayloadType string                  `json:"payloadType"`
	Topics      []string                `mapstructure:"topics"`
}

type TimeValueTypeFloat struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}

type TimeValueTypeString struct {
	Time  int64  `json:"time"`
	Value string `json:"value"`
}

type TimeValueTypeStringArray struct {
	Time  int64    `json:"time"`
	Value []string `json:"value"`
}

type TimeValueTypeStringBool struct {
	Time  int64 `json:"time"`
	Value bool  `json:"value"`
}

func metricName(group string, name string) string {
	result := configuration.Prefix
	if group != "" {
		result += fmt.Sprintf("%s_%s", group, name)
		return result
	} else {
		result += name
		return result
	}
}

func metricHelp(group string, name string) string {
	if group != "" {
		return fmt.Sprintf("new mqttexporter: Name: '%s_%s'", group, name)
	} else {
		return fmt.Sprintf("new mqttexporter: Name: '%s'", name)
	}
}

func metricType(m FiltersEntry) (prometheus.ValueType, error) {
	return prometheus.GaugeValue, nil
}

func metricKey(group string, name string, labels prometheus.Labels) string {
	if group != "" {
		return fmt.Sprintf("%s-%s-%v", group, name, labels)
	} else {
		return fmt.Sprintf("%s-%v", name, labels)
	}
}

type newmqttSample struct {
	Id      string
	Name    string
	Labels  map[string]string
	Help    string
	Value   float64
	DType   string
	Dstype  string
	Time    float64
	Type    prometheus.ValueType
	Unit    string
	Expires time.Time
}

type mqttCollector struct {
	samples map[string]*newmqttSample
	mu      *sync.Mutex
	ch      chan *newmqttSample
}

func newmqttCollector() *mqttCollector {
	c := &mqttCollector{
		ch:      make(chan *newmqttSample, 0),
		mu:      &sync.Mutex{},
		samples: map[string]*newmqttSample{},
	}
	go c.processSamples()
	return c
}

func (c *mqttCollector) processSamples() {
	ticker := time.NewTicker(time.Minute).C
	for {
		select {
		case sample := <-c.ch:
			c.mu.Lock()
			c.samples[sample.Id] = sample
			c.mu.Unlock()
		case <-ticker:
			// Garbage collect expired samples.
			now := time.Now()
			c.mu.Lock()
			for k, sample := range c.samples {
				if now.After(sample.Expires) {
					delete(c.samples, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

func parseValue(value interface{}) (float64, error) {
	svalue := fmt.Sprintf("%v", value)
	val, err := strconv.ParseFloat(svalue, 64)

	if svalue == "false" || svalue == "OFF" {
		return 0, err
	}
	if svalue == "true" || svalue == "ON" {
		return 1, err
	}
	log.Debugf("parseValue: %s - %s", svalue, err)
	if err == nil {
		return val, err
	}
	return -1.0, errors.New("Unvalid value")
}

// Collect implements prometheus.Collector.
func (c mqttCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- lastPush

	c.mu.Lock()
	samples := make([]*newmqttSample, 0, len(c.samples))
	for _, sample := range c.samples {
		samples = append(samples, sample)
	}
	c.mu.Unlock()

	now := time.Now()
	for _, sample := range samples {
		if now.After(sample.Expires) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(sample.Name, sample.Help, []string{}, sample.Labels), sample.Type, sample.Value,
		)
	}
}

// Describe implements prometheus.Collector.
func (c mqttCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- lastPush.Desc()
}

func getParams(regEx *regexp.Regexp, url string) (paramsMap map[string]string) {

	match := regEx.FindStringSubmatch(url)

	paramsMap = make(map[string]string)
	for i, name := range regEx.SubexpNames() {
		if i > 0 && i <= len(match) {
			paramsMap[name] = match[i]
		}
	}
	return paramsMap
}

var messagePubHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	var data = msg.Payload()
	var stData = string(data[:])
	for k, v := range reCache {
		matches := getParams(v.fre, msg.Topic())
		if len(matches) > 0 {
			var filter = configuration.Filters[k]

			if configuration.PayloadType == payloadTypeJson {
				var jsonValue interface{}
				log.Debugf("Received message: %s from topic: %s", stData, msg.Topic())
				err := json.Unmarshal(data, &jsonValue)
				if err == nil {
					for vname, vpath := range filter.Values {
						var name = ""
						for kMatches, vMatches := range matches {
							if kMatches == "N" {
								name = vMatches
							}
						}
						if name == "" {
							name = vname
						}
						var value, _ = jsonpath.Read(jsonValue, vpath)
						if value != nil {
							log.Debugf("Matched filter %s - message: %s from topic: %s => %s - %s = %f", k, stData, msg.Topic(), matches, name, value)

							pvalue, err := parseValue(value)

							var group = configuration.Filters[k].Group

							now := time.Now()
							lastPush.Set(float64(now.UnixNano()) / 1e9)
							metricType, err := metricType(configuration.Filters[k])
							if err == nil {
								labels := prometheus.Labels{}
								for kMatches, vMatches := range matches {
									if kMatches[0] == 'L' {
										labels[kMatches] = vMatches
									}
								}
								collector.ch <- &newmqttSample{
									Id:      metricKey(group, name, labels),
									Name:    metricName(group, name),
									Labels:  labels,
									Help:    metricHelp(group, name),
									Value:   pvalue,
									Type:    metricType,
									Expires: now.Add(time.Duration(300) * time.Second * 2),
								}
							}
						}
					}
				}
			}
		}
	}
}

var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	log.Warnf("Connected")
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	log.Warnf("Connect lost: %v", err)
}

func startExporter() {

	if *verboseVar {
		log.SetLevel(log.DebugLevel)
	}

	configurationFile, err := os.Open(config.Config.ConfigurationFile)
	if err == nil {
		log.Info("Parsing Configuration file")
		byteValue, _ := ioutil.ReadAll(configurationFile)
		json.Unmarshal(byteValue, &configuration)
		if *verboseVar {
			log.Debug(configuration)
		}
		log.Infof("Parsing Configuration file: %d entries", len(configuration.Filters))
		defer configurationFile.Close()
	} else {
		log.Fatalf("Failed to open configuration file: %s", config.Config.ConfigurationFile)
	}

	if configuration.PayloadType != payloadTypeJson {
		log.Fatalf("Wrong PayloadType value: %s", configuration.PayloadType)
	}

	collector = newmqttCollector()
	prometheus.MustRegister(collector)

	log.Info("Listening on " + config.Config.ListeningAddress)
	http.Handle(config.Config.MetricsPath, promhttp.Handler())

	opts := mqtt.NewClientOptions()
	opts.SetClientID(config.Mqtt.ClientId)
	opts.AddBroker(config.Mqtt.Broker)
	opts.SetDefaultPublishHandler(messagePubHandler)
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	}

	log.Info("Compiling filters")
	for k, v := range configuration.Filters {
		c := FilterCache{}
		fre := regexp.MustCompile(v.Filter)
		c.fre = fre
		reCache[k] = c
	}

	log.Infof("Connected to MQTT broker %s", config.Mqtt.Broker)
	for _, v := range configuration.Topics {
		log.Infof("Subscribed to topic %s", v)
		client.Subscribe(v, byte(config.Mqtt.Qos), nil)
	}
	log.Info("Waiting for messages")

	http.ListenAndServe(config.Config.ListeningAddress, nil)
}

func LoadConfig(path string) (err error) {

	pflag.Parse()

	viper.AddConfigPath(path)

	viper.SetConfigFile(configFileVar)

	viper.AutomaticEnv()

	err = viper.ReadInConfig()
	if err != nil {
		return err
	}
	viper.BindPFlags(pflag.CommandLine)
	defaults.SetDefaults(&config)
	err = viper.Unmarshal(&config)

	return err
}

var configFileVar string = "mqtt_exporter.json"
var verboseVar *bool = flag.BoolP("verbose", "v", false, "Verbose mode")

func main() {
	viper.SetEnvPrefix("MQTT_EXPORTER")

	err := LoadConfig(".")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	startExporter()
}
