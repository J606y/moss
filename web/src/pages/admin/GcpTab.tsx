import { useCallback, useEffect, useState } from 'react'
import { PlugZap, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { GcpCredential, GcpSettings } from '../../types'
import { ConfirmDelete, NumberInput, Toggle } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { fmtDateTime } from '../../utils/format'
import { btnPrimary, card, formLabel, iconBtn, input, td, th } from '../../ui'
import type { Toast } from './types'

export function GcpTab({ toast }: { toast: Toast }) {
  const [g, setG] = useState<GcpSettings | null>(null)
  const [saving, setSaving] = useState(false)
  const [saJson, setSaJson] = useState('')
  // 凭证上的按钮按 id 门控：多份凭证并列时，测试第一份不该把第二份的按钮也禁掉。
  // 值形如 'test:abc123'，同一时刻只允许一个凭证在操作中。
  const [credBusy, setCredBusy] = useState('')
  const [deleting, setDeleting] = useState<GcpCredential | null>(null)

  const load = useCallback(() => {
    get<GcpSettings>('/api/admin/gcp')
      .then(setG)
      .catch((e) => toast(errMsg(e)))
  }, [toast])
  useEffect(load, [load])

  if (!g) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: 'delay' | 'cooldown' | 'maxTries') => (v: number) => setG({ ...g, [k]: v })
  const refresh = async () => setG(await get<GcpSettings>('/api/admin/gcp'))

  const onAdd = async () => {
    setCredBusy('add')
    try {
      await post('/api/admin/gcp/credentials', { saJson })
      setSaJson('')
      await refresh()
      toast('凭证已添加')
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onTest = async (c: GcpCredential) => {
    setCredBusy('test:' + c.id)
    try {
      const res = await post<{ clientEmail: string }>(`/api/admin/gcp/credentials/${c.id}/test`, {})
      toast(`连接成功：${res.clientEmail}`)
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onDelete = async (c: GcpCredential) => {
    setDeleting(null)
    setCredBusy('del:' + c.id)
    try {
      await del(`/api/admin/gcp/credentials/${c.id}`)
      await refresh()
      toast('凭证已删除')
    } catch (e) {
      // 被节点占用时后端返回的是一句写明了机器名与出路的话，原样透传
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onSaveSettings = async () => {
    setSaving(true)
    try {
      await put('/api/admin/gcp', {
        autoOn: g.autoOn, delay: g.delay, cooldown: g.cooldown, maxTries: g.maxTries,
      })
      toast('GCP 设置已保存')
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="space-y-4">
      <section className={`${card} mx-auto max-w-4xl space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">添加 Service Account 凭证</h3>
        <div>
          <label className={formLabel}>Service Account JSON 密钥</label>
          <textarea
            className={`${input} min-h-28 font-mono text-xs`}
            placeholder='{ "type": "service_account", ... }'
            value={saJson}
            onChange={(e) => setSaJson(e.target.value)}
          />
          <p className="mt-1 text-xs text-zinc-400">
            只需授予 compute.instances.get 与 compute.instances.start 两个权限；私钥保存后不再回显。
            多个 GCP 账号各添加一份，再到「服务器」页为每台节点选择使用哪一份。
          </p>
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onAdd} disabled={credBusy !== '' || !saJson.trim()}>
            {credBusy === 'add' ? '保存中…' : '添加凭证'}
          </button>
        </div>
      </section>

      <section className={`${card} mx-auto max-w-4xl p-4`}>
        <h3 className="mb-3 text-sm font-semibold">凭证列表</h3>
        {g.credentials.length === 0 ? (
          <p className="py-8 text-center text-sm text-zinc-400">
            还没有凭证。在上方粘贴 Service Account JSON 添加一份。
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                {/* 次要列窄屏隐藏：手机上要能直接够到测试/删除，而不是横滑一段才摸得到 */}
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>项目 ID</th>
                  <th className={`${th} hidden md:table-cell`}>Service Account</th>
                  <th className={th}>使用中</th>
                  <th className={`${th} hidden lg:table-cell`}>添加时间</th>
                  <th className={th}></th>
                </tr>
              </thead>
              <tbody>
                {g.credentials.map((c) => (
                  <tr key={c.id} className="border-b border-white/25 dark:border-white/5">
                    <td className={td}>
                      <div className="flex items-center gap-2">
                        {c.projectId}
                        {!c.decryptable && (
                          <span
                            className="rounded bg-rose-500/10 px-1.5 py-0.5 text-xs text-rose-500"
                            title="密文还在，但用当前主密钥解不开。恢复原 MOSS_SECRET_KEY / secret.key 即可救回；删掉重加会永久失去它。"
                          >
                            无法解密
                          </span>
                        )}
                      </div>
                    </td>
                    {/* 邮箱形如 moss-starter@<项目>.iam.gserviceaccount.com，能有 60 字符。
                        不截断的话它会把整张表撑出容器，操作按钮被挤到横向滚动区外——
                        看得见凭证却点不到删除。 */}
                    <td
                      className={`${td} hidden max-w-[15rem] truncate font-mono text-xs text-zinc-500 md:table-cell`}
                      title={c.clientEmail}
                    >
                      {c.clientEmail}
                    </td>
                    <td className={td}>
                      {c.serverCount > 0 ? (
                        `${c.serverCount} 台节点`
                      ) : (
                        <span className="text-zinc-400">未使用</span>
                      )}
                    </td>
                    <td className={`${td} hidden lg:table-cell`}>{fmtDateTime(c.createdAt * 1000)}</td>
                    <td className={`${td} text-right`}>
                      <div className="flex justify-end gap-1">
                        <button
                          className={iconBtn}
                          title="测试连接"
                          onClick={() => onTest(c)}
                          disabled={credBusy !== ''}
                        >
                          <PlugZap className="h-4 w-4" />
                        </button>
                        <button
                          className={iconBtn}
                          title="删除"
                          onClick={() => setDeleting(c)}
                          disabled={credBusy !== ''}
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {g.unboundCount > 0 && (
          <p className="mt-3 text-xs text-amber-500/90">
            有 {g.unboundCount} 台节点开启了自动开机但未绑定凭证，请到「服务器」页为它们选择凭证。
          </p>
        )}
      </section>

      <section className={`${card} mx-auto max-w-4xl space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">自动开机</h3>
        <Toggle
          checked={g.autoOn}
          label="节点离线后自动调用 GCP API 开机（总开关）"
          onChange={(v) => setG({ ...g, autoOn: v })}
        />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>离线确认延迟（秒，60 ~ 3600）</label>
            <NumberInput min={60} max={3600} value={g.delay} onChange={num('delay')} />
          </div>
          <div>
            <label className={formLabel}>重试冷却（秒，60 ~ 3600）</label>
            <NumberInput min={60} max={3600} value={g.cooldown} onChange={num('cooldown')} />
          </div>
          <div>
            <label className={formLabel}>最大尝试次数（1 ~ 10）</label>
            <NumberInput min={1} max={10} value={g.maxTries} onChange={num('maxTries')} />
          </div>
        </div>
        <p className="text-xs text-zinc-400">
          节点确认离线后查询实例状态，仅在 TERMINATED（Spot 被抢占）时调用开机；实例 RUNNING
          但节点离线（agent/网络故障）不会重复开机。达到最大尝试次数后停止并通知，节点重新上线自动复位计数。
          需在「服务器」页对具体节点开启 GCP 自动开机并选择凭证、填写 zone / 实例名。
        </p>
        <p className="text-xs text-amber-500/90">
          注意：面板本身请勿部署在被守护的 Spot 实例上；人为关机维护前请先关闭对应节点的自动开机开关，否则会被自动拉起。
        </p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onSaveSettings} disabled={saving || credBusy !== ''}>
            {saving ? '保存中…' : '保存设置'}
          </button>
        </div>
      </section>

      {deleting && (
        <ConfirmDelete
          title="删除凭证"
          onCancel={() => setDeleting(null)}
          onConfirm={() => onDelete(deleting)}
        >
          将删除 <b>{deleting.projectId}</b>（{deleting.clientEmail}）。
          {deleting.serverCount > 0
            ? ` 当前有 ${deleting.serverCount} 台节点正在使用它，需要先关闭这些节点的自动开机或改绑其他凭证。`
            : ' 删除后需重新粘贴 JSON 才能恢复。'}
        </ConfirmDelete>
      )}
    </div>
  )
}
