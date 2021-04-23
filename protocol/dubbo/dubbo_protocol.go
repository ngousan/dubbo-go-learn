package dubbo

import (
	"context"
	"fmt"
	"github.com/laz/dubbo-go/remoting/getty"

	"github.com/laz/dubbo-go/common"
	"github.com/laz/dubbo-go/common/constant"
	"github.com/laz/dubbo-go/common/extension"
	"github.com/laz/dubbo-go/common/logger"
	"github.com/laz/dubbo-go/protocol"
	"github.com/laz/dubbo-go/protocol/invocation"
	"github.com/laz/dubbo-go/remoting"
	"sync"
)
import (
	"github.com/opentracing/opentracing-go"
)

const (
	// DUBBO is dubbo protocol name
	DUBBO = "dubbo"
)

func init() {
	extension.SetProtocol(DUBBO, GetProtocol)
}

var (
	dubboProtocol *DubboProtocol
)

// It support dubbo protocol. It implements Protocol interface for dubbo protocol.
type DubboProtocol struct {
	protocol.BaseProtocol
	// It is store relationship about serviceKey(group/interface:version) and ExchangeServer
	// The ExchangeServer is introduced to replace of Server. Because Server is depend on getty directly.
	serverMap  map[string]*remoting.ExchangeServer
	serverLock sync.Mutex
}

// GetProtocol get a single dubbo protocol.
func GetProtocol() protocol.Protocol {
	if dubboProtocol == nil {
		dubboProtocol = NewDubboProtocol()
	}
	return dubboProtocol
}

// Export export dubbo service.
func (dp *DubboProtocol) Export(invoker protocol.Invoker) protocol.Exporter {
	url := invoker.GetUrl()
	serviceKey := url.ServiceKey()
	exporter := NewDubboExporter(serviceKey, invoker, dp.ExporterMap())
	dp.SetExporterMap(serviceKey, exporter)
	logger.Infof("Export service: %s", url.String())
	// start server
	dp.openServer(url)
	return exporter

}

//开启rpc服务器
func (dp *DubboProtocol) openServer(url *common.URL) {
	_, ok := dp.serverMap[url.Location]
	if !ok {
		_, ok := dp.ExporterMap().Load(url.ServiceKey())
		if !ok {
			panic("[DubboProtocol]" + url.Key() + "is not existing")
		}

		dp.serverLock.Lock()
		_, ok = dp.serverMap[url.Location]
		if !ok {
			handler := func(invocation *invocation.RPCInvocation) protocol.RPCResult {
				return doHandleRequest(invocation)
			}
			srv := remoting.NewExchangeServer(url, getty.NewServer(url, handler))
			dp.serverMap[url.Location] = srv
			srv.Start()
		}
		dp.serverLock.Unlock()
	}
}

func doHandleRequest(rpcInvocation *invocation.RPCInvocation) protocol.RPCResult {
	exporter, _ := dubboProtocol.ExporterMap().Load(rpcInvocation.ServiceKey())
	result := protocol.RPCResult{}
	if exporter == nil {
		err := fmt.Errorf("don't have this exporter, key: %s", rpcInvocation.ServiceKey())
		logger.Errorf(err.Error())
		result.Err = err
		//reply(session, p, hessian.PackageResponse)
		return result
	}
	invoker := exporter.(protocol.Exporter).GetInvoker()
	if invoker != nil {
		// FIXME
		ctx := rebuildCtx(rpcInvocation)

		invokeResult := invoker.Invoke(ctx, rpcInvocation)
		if err := invokeResult.Error(); err != nil {
			result.Err = invokeResult.Error()
			//p.Header.ResponseStatus = hessian.Response_OK
			//p.Body = hessian.NewResponse(nil, err, result.Attachments())
		} else {
			result.Rest = invokeResult.Result()
			//p.Header.ResponseStatus = hessian.Response_OK
			//p.Body = hessian.NewResponse(res, nil, result.Attachments())
		}
	} else {
		result.Err = fmt.Errorf("don't have the invoker, key: %s", rpcInvocation.ServiceKey())
	}
	return result
}

// rebuildCtx rebuild the context by attachment.
// Once we decided to transfer more context's key-value, we should change this.
// now we only support rebuild the tracing context
func rebuildCtx(inv *invocation.RPCInvocation) context.Context {
	ctx := context.WithValue(context.Background(), constant.DubboCtxKey("attachment"), inv.Attachments())

	// actually, if user do not use any opentracing framework, the err will not be nil.
	spanCtx, err := opentracing.GlobalTracer().Extract(opentracing.TextMap,
		opentracing.TextMapCarrier(filterContext(inv.Attachments())))
	if err == nil {
		ctx = context.WithValue(ctx, constant.DubboCtxKey(constant.TRACING_REMOTE_SPAN_CTX), spanCtx)
	}
	return ctx
}

// NewDubboExporter get a DubboExporter.
func NewDubboExporter(key string, invoker protocol.Invoker, exporterMap *sync.Map) *DubboExporter {
	return &DubboExporter{
		BaseExporter: *protocol.NewBaseExporter(key, invoker, exporterMap),
	}
}

// Refer create dubbo service reference.
func (dp *DubboProtocol) Refer(url *common.URL) protocol.Invoker {
	return nil
}

// Destroy destroy dubbo service.
func (dp *DubboProtocol) Destroy() {
	logger.Infof("DubboProtocol destroy.")

}

// NewDubboProtocol create a dubbo protocol.
func NewDubboProtocol() *DubboProtocol {
	return &DubboProtocol{
		BaseProtocol: protocol.NewBaseProtocol(),
		serverMap:    make(map[string]*remoting.ExchangeServer),
	}
}
