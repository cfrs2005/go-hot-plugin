// 厂商 B 插件：调 B 的 {"goods_name","price"} 接口并映射成统一结构。
package main

import (
	"context"
	"net/url"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

func main() {
	pluginapi.Serve("B", "v1", func(ctx context.Context, base string, req pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
		var v struct {
			GoodsName string `json:"goods_name"`
			Price     string `json:"price"`
		}
		if err := pluginapi.GetJSON(ctx, base+"/vendorB/goods/"+url.PathEscape(req.ID)+pluginapi.DelayQuery(req.DelayMS), &v); err != nil {
			return pluginapi.QueryResponse{}, err
		}
		cents, err := pluginapi.ParseDecimalCents(v.Price)
		if err != nil {
			return pluginapi.QueryResponse{}, err
		}
		return pluginapi.QueryResponse{ID: req.ID, Name: v.GoodsName, PriceCents: cents}, nil
	})
}
