package config

import (
	"container/list"
	"fmt"
	"github.com/creasty/defaults"
	gxnet "github.com/dubbogo/gost/net"
	"github.com/laz/dubbo-go/common"
	"github.com/laz/dubbo-go/common/constant"
	"github.com/laz/dubbo-go/common/extension"
	_ "github.com/laz/dubbo-go/common/extension"
	"github.com/laz/dubbo-go/common/logger"
	"github.com/laz/dubbo-go/protocol"
	perrors "github.com/pkg/errors"
	"go.uber.org/atomic"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServiceConfig is the configuration of the service provider
type ServiceConfig struct {
	Methods                     []*MethodConfig `yaml:"methods"  json:"methods,omitempty" property:"methods"`
	InterfaceName               string          `required:"true"  yaml:"interface"  json:"interface,omitempty" property:"interface"`
	id                          string
	rpcService                  common.RPCService
	Protocols                   map[string]*ProtocolConfig
	Registry                    string            `yaml:"registry"  json:"registry,omitempty"  property:"registry"`
	Params                      map[string]string `yaml:"params"  json:"params,omitempty" property:"params"`
	Cluster                     string            `default:"failover" yaml:"cluster"  json:"cluster,omitempty" property:"cluster"`
	Loadbalance                 string            `default:"random" yaml:"loadbalance"  json:"loadbalance,omitempty"  property:"loadbalance"`
	Warmup                      string            `yaml:"warmup"  json:"warmup,omitempty"  property:"warmup"`
	Group                       string            `yaml:"group"  json:"group,omitempty" property:"group"`
	Version                     string            `yaml:"version"  json:"version,omitempty" property:"version" `
	GrpcMaxMessageSize          int               `default:"4" yaml:"max_message_size" json:"max_message_size,omitempty"`
	Retries                     string            `yaml:"retries"  json:"retries,omitempty" property:"retries"`
	Serialization               string            `yaml:"serialization" json:"serialization" property:"serialization"`
	Filter                      string            `yaml:"filter" json:"filter,omitempty" property:"filter"`
	AccessLog                   string            `yaml:"accesslog" json:"accesslog,omitempty" property:"accesslog"`
	TpsLimiter                  string            `yaml:"tps.limiter" json:"tps.limiter,omitempty" property:"tps.limiter"`
	TpsLimitInterval            string            `yaml:"tps.limit.interval" json:"tps.limit.interval,omitempty" property:"tps.limit.interval"`
	TpsLimitRate                string            `yaml:"tps.limit.rate" json:"tps.limit.rate,omitempty" property:"tps.limit.rate"`
	TpsLimitStrategy            string            `yaml:"tps.limit.strategy" json:"tps.limit.strategy,omitempty" property:"tps.limit.strategy"`
	TpsLimitRejectedHandler     string            `yaml:"tps.limit.rejected.handler" json:"tps.limit.rejected.handler,omitempty" property:"tps.limit.rejected.handler"`
	ExecuteLimit                string            `yaml:"execute.limit" json:"execute.limit,omitempty" property:"execute.limit"`
	ExecuteLimitRejectedHandler string            `yaml:"execute.limit.rejected.handler" json:"execute.limit.rejected.handler,omitempty" property:"execute.limit.rejected.handler"`
	Auth                        string            `yaml:"auth" json:"auth,omitempty" property:"auth"`
	ParamSign                   string            `yaml:"param.sign" json:"param.sign,omitempty" property:"param.sign"`
	Tag                         string            `yaml:"tag" json:"tag,omitempty" property:"tag"`
	export                      bool              // a flag to control whether the current service should export or not
	Protocol                    string            `default:"dubbo"  required:"true"  yaml:"protocol"  json:"protocol,omitempty" property:"protocol"` // multi protocol support, split by ','
	Token                       string            `yaml:"token" json:"token,omitempty" property:"token"`
	exported                    *atomic.Bool
	cacheMutex                  sync.Mutex
	cacheProtocol               protocol.Protocol
	exporters                   []protocol.Exporter
	unexported                  *atomic.Bool
}

//制定此方法，yaml框架解析时或走该方法进行解析
func (c *ServiceConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := defaults.Set(c); err != nil {
		return err
	}
	type plain ServiceConfig
	if err := unmarshal((*plain)(c)); err != nil {
		return err
	}
	c.exported = atomic.NewBool(false)
	c.unexported = atomic.NewBool(false)
	c.export = true
	return nil
}

// Implement only store the @s and return
func (c *ServiceConfig) Implement(s common.RPCService) {
	c.rpcService = s
}

// Export exports the service
func (c *ServiceConfig) Export() error {

	regUrls := loadRegistries(c.Registry, providerConfig.Registries, common.PROVIDER)
	urlMap := c.getUrlMap()
	protocolConfigs := loadProtocol(c.Protocol, c.Protocols)
	if len(protocolConfigs) == 0 {
		logger.Warnf("The service %v's '%v' protocols don't has right protocolConfigs", c.InterfaceName, c.Protocol)
		return nil
	}
	ports := getRandomPort(protocolConfigs)
	nextPort := ports.Front()
	proxyFactory := extension.GetProxyFactory(providerConfig.ProxyFactory)
	for _, proto := range protocolConfigs {
		// registry the service reflect
		methods, err := common.ServiceMap.Register(c.InterfaceName, proto.Name, c.Group, c.Version, c.rpcService)
		if err != nil {
			formatErr := perrors.Errorf("The service %v export the protocol %v error! Error message is %v.",
				c.InterfaceName, proto.Name, err.Error())
			logger.Errorf(formatErr.Error())
			return formatErr
		}

		port := proto.Port
		if len(proto.Port) == 0 {
			port = nextPort.Value.(string)
			nextPort = nextPort.Next()
		}
		ivkURL := common.NewURLWithOptions(
			common.WithPath(c.InterfaceName),
			common.WithProtocol(proto.Name),
			common.WithIp(proto.Ip),
			common.WithPort(port),
			common.WithParams(urlMap),
			common.WithParamsValue(constant.BEAN_NAME_KEY, c.id),
			common.WithParamsValue(constant.SSL_ENABLED_KEY, strconv.FormatBool(GetSslEnabled())),
			common.WithMethods(strings.Split(methods, ",")),
			common.WithToken(c.Token),
		)
		if len(c.Tag) > 0 {
			ivkURL.AddParam(constant.Tagkey, c.Tag)
		}
		// post process the URL to be exported
		c.postProcessConfig(ivkURL)
		// config post processor may set "export" to false
		if !ivkURL.GetParamBool(constant.EXPORT_KEY, true) {
			return nil
		}
		if len(regUrls) > 0 {
			c.cacheMutex.Lock()
			if c.cacheProtocol == nil {
				logger.Infof(fmt.Sprintf("First load the registry protocol, url is {%v}!", ivkURL))
				c.cacheProtocol = extension.GetProtocol("registry")
			}
			c.cacheMutex.Unlock()
			for _, regUrl := range regUrls {
				regUrl.SubURL = ivkURL
				invoker := proxyFactory.GetInvoker(regUrl)
				exporter := c.cacheProtocol.Export(invoker)
				if exporter == nil {
					return perrors.New(fmt.Sprintf("Registry protocol new exporter error, registry is {%v}, url is {%v}", regUrl, ivkURL))
				}
				c.exporters = append(c.exporters, exporter)
			}
		}
		//发布服务定义信息到元数据中心
		publishServiceDefinition(ivkURL)
	}
	c.exported.Store(true)
	return nil
}
func publishServiceDefinition(url *common.URL) {
	if remoteMetadataService, err := extension.GetRemoteMetadataService(); err == nil && remoteMetadataService != nil {
		remoteMetadataService.PublishServiceDefinition(url)
	}
}

// Get Random Port
func getRandomPort(protocolConfigs []*ProtocolConfig) *list.List {
	ports := list.New()
	for _, proto := range protocolConfigs {
		if len(proto.Port) > 0 {
			continue
		}

		tcp, err := gxnet.ListenOnTCPRandomPort(proto.Ip)
		if err != nil {
			panic(perrors.New(fmt.Sprintf("Get tcp port error, err is {%v}", err)))
		}
		defer tcp.Close()
		ports.PushBack(strings.Split(tcp.Addr().String(), ":")[1])
	}
	return ports
}

func (c *ServiceConfig) getUrlMap() url.Values {
	urlMap := url.Values{}
	// first set user params
	for k, v := range c.Params {
		urlMap.Set(k, v)
	}
	urlMap.Set(constant.INTERFACE_KEY, c.InterfaceName)
	urlMap.Set(constant.TIMESTAMP_KEY, strconv.FormatInt(time.Now().Unix(), 10))
	urlMap.Set(constant.CLUSTER_KEY, c.Cluster)
	urlMap.Set(constant.LOADBALANCE_KEY, c.Loadbalance)
	urlMap.Set(constant.WARMUP_KEY, c.Warmup)
	urlMap.Set(constant.RETRIES_KEY, c.Retries)
	urlMap.Set(constant.GROUP_KEY, c.Group)
	urlMap.Set(constant.VERSION_KEY, c.Version)
	urlMap.Set(constant.ROLE_KEY, strconv.Itoa(common.PROVIDER))
	urlMap.Set(constant.RELEASE_KEY, "dubbo-golang-"+constant.Version)
	urlMap.Set(constant.SIDE_KEY, (common.RoleType(common.PROVIDER)).Role())
	urlMap.Set(constant.MESSAGE_SIZE_KEY, strconv.Itoa(c.GrpcMaxMessageSize))
	// todo: move
	urlMap.Set(constant.SERIALIZATION_KEY, c.Serialization)
	// application info
	urlMap.Set(constant.APPLICATION_KEY, providerConfig.ApplicationConfig.Name)
	urlMap.Set(constant.ORGANIZATION_KEY, providerConfig.ApplicationConfig.Organization)
	urlMap.Set(constant.NAME_KEY, providerConfig.ApplicationConfig.Name)
	urlMap.Set(constant.MODULE_KEY, providerConfig.ApplicationConfig.Module)
	urlMap.Set(constant.APP_VERSION_KEY, providerConfig.ApplicationConfig.Version)
	urlMap.Set(constant.OWNER_KEY, providerConfig.ApplicationConfig.Owner)
	urlMap.Set(constant.ENVIRONMENT_KEY, providerConfig.ApplicationConfig.Environment)

	// filter
	urlMap.Set(constant.SERVICE_FILTER_KEY, mergeValue(providerConfig.Filter, c.Filter, constant.DEFAULT_SERVICE_FILTERS))

	// filter special config
	urlMap.Set(constant.ACCESS_LOG_KEY, c.AccessLog)
	// tps limiter
	urlMap.Set(constant.TPS_LIMIT_STRATEGY_KEY, c.TpsLimitStrategy)
	urlMap.Set(constant.TPS_LIMIT_INTERVAL_KEY, c.TpsLimitInterval)
	urlMap.Set(constant.TPS_LIMIT_RATE_KEY, c.TpsLimitRate)
	urlMap.Set(constant.TPS_LIMITER_KEY, c.TpsLimiter)
	urlMap.Set(constant.TPS_REJECTED_EXECUTION_HANDLER_KEY, c.TpsLimitRejectedHandler)

	// execute limit filter
	urlMap.Set(constant.EXECUTE_LIMIT_KEY, c.ExecuteLimit)
	urlMap.Set(constant.EXECUTE_REJECTED_EXECUTION_HANDLER_KEY, c.ExecuteLimitRejectedHandler)

	// auth filter
	urlMap.Set(constant.SERVICE_AUTH_KEY, c.Auth)
	urlMap.Set(constant.PARAMETER_SIGNATURE_ENABLE_KEY, c.ParamSign)

	// whether to export or not
	urlMap.Set(constant.EXPORT_KEY, strconv.FormatBool(c.export))

	for _, v := range c.Methods {
		prefix := "methods." + v.Name + "."
		urlMap.Set(prefix+constant.LOADBALANCE_KEY, v.LoadBalance)
		urlMap.Set(prefix+constant.RETRIES_KEY, v.Retries)
		urlMap.Set(prefix+constant.WEIGHT_KEY, strconv.FormatInt(v.Weight, 10))

		urlMap.Set(prefix+constant.TPS_LIMIT_STRATEGY_KEY, v.TpsLimitStrategy)
		urlMap.Set(prefix+constant.TPS_LIMIT_INTERVAL_KEY, v.TpsLimitInterval)
		urlMap.Set(prefix+constant.TPS_LIMIT_RATE_KEY, v.TpsLimitRate)

		urlMap.Set(constant.EXECUTE_LIMIT_KEY, v.ExecuteLimit)
		urlMap.Set(constant.EXECUTE_REJECTED_EXECUTION_HANDLER_KEY, v.ExecuteLimitRejectedHandler)
	}

	return urlMap
}

// postProcessConfig asks registered ConfigPostProcessor to post-process the current ServiceConfig.
func (c *ServiceConfig) postProcessConfig(url *common.URL) {

}
