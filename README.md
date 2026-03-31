## Cursor Byok API Server
cursor-byok-server 是 Cursor助手的Pro模式的服务端。无限量支持tab和byok,支持自定义API使用Cursor。

#### 前提和原理

其原理是通过一个**具有byok资格的token**将「Cursor tab」流量和「Cursor Agent」流量转发到官方服务。


其中「Cursor Agent」强制byok，你必须在客户端转发中实现特定特征并告知私有模型配置信息。才允许通过agent服务。

- [bilibili 主页](https://space.bilibili.com/311706663) 
- [客户端（未开源）](https://dcne38qm5vlg.feishu.cn/wiki/YGaWw1ejXiiJ8EkismtcoIYUnNd) 

#### 注意
本服务是为Cursor助手中的一个模式的服务端转发，Cursor助手的核心能力在于**本地模式**，此服务作为中期临时过渡，当**本地模式**生产可用时，此服务将被弃用

#### 构建和部署
- 仅推荐Docker构建和部署。
