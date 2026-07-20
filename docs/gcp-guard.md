# GCP Spot 自动开机 · 手把手教程

> 对应 README 的[「GCP Spot 自动开机」](../README.md#gcp-spot-自动开机可选)小节,这里是展开版:每一步点哪里、填什么、怎么验证,以及常见问题。

GCP 的 Spot 实例便宜(通常是按需价的 1~3 折),代价是随时可能被抢占——实例变成 `TERMINATED`(已停止),磁盘数据不丢,但机器不会自己爬起来。Moss 可以充当看门狗:节点确认离线后自动调用 Compute Engine API 把实例重新开机,整个过程和结果都会推送 Telegram。

```
节点离线 ──(等「确认延迟」,默认 120s)──▶ 查询实例状态
                                          ├─ TERMINATED ──▶ 调用 start 开机 ──失败──▶ 冷却后重试,超过次数上限则放弃并通知
                                          └─ RUNNING    ──▶ 不动实例,只提醒(是 agent / 网络问题)
节点重新上线 ──▶ 重试计数自动清零
```

## 0. 开始前确认

- Moss **v1.1.0 及以上**,面板已部署并能正常收到节点上报;
- **面板没有跑在要守护的 Spot 实例上**——面板和实例一起被抢占,就没人拉起谁了;
- Spot 实例创建时「终止操作」保持默认的**停止(STOP)**。若设成「删除(DELETE)」,被抢占后实例直接消失,任何工具都救不回来;
- 建议先配好 Telegram 通知(后台「通知」页),开机成功 / 失败 / 放弃都靠它告诉你。

## 1. 创建最小权限的 Service Account

守护只需要「查实例状态」和「开机」两个权限,推荐建一个专用的最小权限账号,泄露了也翻不起浪。

打开 [GCP 控制台](https://console.cloud.google.com/),点右上角的 **Cloud Shell** 图标(`>_`),把下面整段粘贴进去执行(只需把第一行换成你的项目 ID):

```bash
PROJECT=你的项目ID

# 1) 创建专用 Service Account
gcloud iam service-accounts create moss-starter --project=$PROJECT

# 2) 创建只含两个权限的自定义角色
gcloud iam roles create mossSpotStarter --project=$PROJECT \
  --permissions=compute.instances.get,compute.instances.start

# 3) 把角色绑给这个账号
gcloud projects add-iam-policy-binding $PROJECT \
  --member="serviceAccount:moss-starter@$PROJECT.iam.gserviceaccount.com" \
  --role="projects/$PROJECT/roles/mossSpotStarter"

# 4) 生成密钥文件
gcloud iam service-accounts keys create moss-sa.json \
  --iam-account=moss-starter@$PROJECT.iam.gserviceaccount.com
```

看到 `created key [...] as [moss-sa.json]` 即成功。然后:

```bash
cat moss-sa.json
```

把打印出来的**整段 JSON**(从 `{` 到 `}`)复制下来,下一步要用。粘贴进面板后回来把文件删掉:

```bash
rm moss-sa.json
```

> 偷懒也可以跳过自定义角色,直接绑预定义角色 `roles/compute.instanceAdmin.v1`,但它还能删机器、改配置,权限过宽,不推荐。

## 2. 面板全局配置(「GCP 守护」页)

进入 Moss 管理后台 → **GCP 守护**:

1. 把上一步复制的 JSON 粘贴进凭证框;
2. 点**保存并测试连接**——面板会真实拿凭证换一次 token,通过会显示账号邮箱和项目 ID;
3. 打开**自动开机**总开关。

三个参数一般保持默认即可:

| 参数 | 默认 | 含义 |
| --- | --- | --- |
| 确认延迟 | 120s | 节点离线多久后才去查实例状态,避免网络抖动误开机 |
| 冷却 | 300s | 开机失败后隔多久重试(Spot 容量不足时很常见) |
| 最大尝试次数 | 3 | 连续失败几次后放弃并通知;节点重新上线自动清零 |

## 3. 逐台节点配置(「服务器」页编辑弹窗)

凭证是全局一份,每台被守护的节点单独填自己的位置信息。到**服务器**页,编辑对应节点:

1. 勾选**GCP 自动开机**;
2. **Zone**:填实例所在可用区,如 `asia-east2-a`。在 GCP 控制台「Compute Engine → 虚拟机实例」列表的「可用区」列能看到——注意是 zone(带 `-a`/`-b` 后缀),不是 region;
3. **实例名**:填 GCP 实例列表**第一列的名称**,必须完全一致。这是 GCP 里的实例名,不是 Moss 里的节点名;
4. **项目 ID**:留空即用凭证里的 `project_id`,绝大多数情况留空即可(跨项目见下文)。

有多台 Spot 就逐台重复,同一项目里的实例共用同一份凭证,GCP 侧无需任何额外操作。

## 4. 验证

保存后,节点行会出现 **▶** 手动开机按钮。点一下:

- 实例正在运行 → 提示「正在运行,无需操作」——**这就是想要的结果**,说明凭证、zone、实例名整条链路都通了;
- 实例处于停止状态 → 会真实开机,等一两分钟节点应重新上线;
- 报错 → 见下方[故障排查](#故障排查)。

▶ 按钮不受总开关限制,随时可用;自动守护则需要总开关和节点开关同时打开。

## 跨项目守护

实例分散在多个项目时,不用换凭证——把同一个 Service Account 授权到其他项目即可。在 Cloud Shell 执行(`OTHER` 换成另一个项目 ID,`SA_PROJECT` 换成凭证所在项目 ID):

```bash
OTHER=另一个项目ID
SA_PROJECT=凭证所在项目ID
gcloud iam roles create mossSpotStarter --project=$OTHER \
  --permissions=compute.instances.get,compute.instances.start
gcloud projects add-iam-policy-binding $OTHER \
  --member="serviceAccount:moss-starter@$SA_PROJECT.iam.gserviceaccount.com" \
  --role="projects/$OTHER/roles/mossSpotStarter"
```

然后在对应节点的「项目 ID」字段填上 `OTHER` 的项目 ID。

## 常见问题

**要人为关机维护,怎么不被自动拉起?**
先关掉该节点的「GCP 自动开机」开关(或总开关),再关机;维护完记得开回来。

**更换项目的结算账号,守护要重新配吗?**
不用。结算账号只管扣费,和 IAM / 凭证无关,换绑后一切照旧。唯一注意:别让项目中途处于「无结算账号」状态,那会导致 Compute API 停用、实例被停机——这种账务性停机守护也救不了(start 会一直失败)。

**实例明明 RUNNING,面板却提示节点离线?**
这不是 GCP 的问题,是 agent 挂了或网络不通。Moss 这种情况只发提醒、不碰实例——盲目重启一台正在跑业务的机器比不动更危险。上机检查 agent 服务即可。

**开机一直失败直到放弃,一般是什么原因?**
最常见是该 zone 的 Spot 容量不足(错误信息含 `ZONE_RESOURCE_POOL_EXHAUSTED`),过段时间手动 ▶ 重试,或考虑换 zone 重建;也可能是项目账务出问题(结算账号失效 / 被暂停)。通知里会带上 GCP 返回的错误信息。

**实例是 `SUSPENDED`(挂起)状态,能自动恢复吗?**
暂不支持,目前只处理 `TERMINATED`。挂起的实例请到 GCP 控制台手动恢复。

**凭证安全吗?**
凭证以明文存在面板的 SQLite 数据库里(单管理员场景下的取舍)。所以务必:只授「查状态 + 开机」两个权限、只绑定必要的项目、面板管理密码设强一点。想作废凭证,到 GCP 控制台「IAM → 服务账号」删掉对应密钥或整个账号即可,面板里再粘一份新的。

**支持 AWS / Azure / 其他云吗?**
暂不支持,目前只有 GCP。

## 故障排查

| 现象 | 原因与处理 |
| --- | --- |
| 保存并测试连接报错 | JSON 粘贴不完整(必须从 `{` 到 `}`),或 Service Account 被删 / 密钥被吊销——重新生成密钥再粘贴 |
| ▶ 报 404 | zone 或实例名填错,项目 ID 不对也会 404——对照 GCP 实例列表逐项核对 |
| ▶ 报 403 | 权限不够:角色没绑上,或跨项目节点没做上面的跨项目授权 |
| 被抢占了但一直没自动开机 | 依次检查:总开关开了吗 → 节点的开关开了吗 → 离线时长过「确认延迟」了吗 → 通知里有没有「已放弃」(重试次数用尽,节点上线前不会再试,可手动 ▶) |
| 实例被抢占后直接消失了 | 「终止操作」被设成了 DELETE,只能重建实例,重建时改回 STOP |
