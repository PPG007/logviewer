// 远端主机连接表单（M9）：主机/端口/用户/认证方式，连接成功后打开目录浏览器。
//
// 凭据约定：口令随连接提交给后端，**连接成功后会明文保存在本机数据库**，
// 之后连接不必再输入（编辑表单不回填口令，留空即沿用已保存的）。
// 前端不持有已保存的口令：后端只回传「是否已保存」，连接时由后端取用。
// 表单校验沿用仓库既有风格（受控 state + 提交时校验 + message.error），不引入 antd Form。

import { useEffect, useState } from 'react'
import { App, Button, Input, InputNumber, Modal, Select, Space, Typography } from 'antd'
import { FolderOpenOutlined } from '@ant-design/icons'
import { api, errText } from '../api'
import { refreshConnections } from '../store/useStore'
import type { Connection, Credential } from '../types'

const AUTH_OPTIONS = [
  { value: 'password', label: '密码' },
  { value: 'key', label: '私钥文件' },
]

// 本地缓存三态：跟随全局 / 强制开 / 强制关
const CACHE_OPTIONS = [
  { value: 'inherit', label: '跟随全局设置' },
  { value: 'on', label: '启用' },
  { value: 'off', label: '不启用' },
]

interface Props {
  open: boolean
  /** 非 null 表示编辑已有主机（口令仍需重新输入）。 */
  connection: Connection | null
  onClose: () => void
  /** 连接成功后回调（父组件据此打开目录浏览器）。 */
  onConnected: (c: Connection) => void
}

export default function ConnectModal({ open, connection, onClose, onConnected }: Props) {
  const { message } = App.useApp()
  const editing = connection !== null

  const [name, setName] = useState('')
  const [host, setHost] = useState('')
  const [port, setPort] = useState(22)
  const [user, setUser] = useState('')
  const [authMethod, setAuthMethod] = useState<'password' | 'key'>('password')
  const [keyPath, setKeyPath] = useState('')
  const [cacheMode, setCacheMode] = useState<'inherit' | 'on' | 'off'>('inherit')
  const [secret, setSecret] = useState('')
  // 是否已保存口令（清除后就地更新，避免为了刷新一个标记去动父组件的流程）
  const [hasSaved, setHasSaved] = useState(false)
  const [loading, setLoading] = useState(false)

  // 每次打开时按当前主机重置表单（新建则清空）
  useEffect(() => {
    if (!open) return
    setName(connection?.Name ?? '')
    setHost(connection?.Host ?? '')
    setPort(connection?.Port || 22)
    setUser(connection?.User ?? '')
    setAuthMethod((connection?.AuthMethod as 'password' | 'key') || 'password')
    setKeyPath(connection?.KeyPath ?? '')
    setCacheMode(connection?.Cache == null ? 'inherit' : connection.Cache ? 'on' : 'off')
    setHasSaved(connection?.HasPassword ?? false)
    setSecret('') // 口令不回填：留空表示沿用已保存的
  }, [open, connection])

  const pickKey = async () => {
    try {
      const p = await api.pickKeyFile()
      if (p) setKeyPath(p)
    } catch (e) {
      message.error(`选择私钥文件失败：${errText(e)}`)
    }
  }

  const submit = async () => {
    if (!host.trim()) {
      message.error('请填写主机地址')
      return
    }
    if (!user.trim()) {
      message.error('请填写用户名')
      return
    }
    if (!port || port < 1 || port > 65535) {
      message.error('端口需在 1~65535 之间')
      return
    }
    // 已保存过口令时允许留空（沿用已保存的）；从未保存过则必须输入
    if (authMethod === 'password' && !secret && !hasSaved) {
      message.error('请输入登录密码')
      return
    }

    setLoading(true)
    // 先落库拿到 id：远端会话、目录浏览、历史记录都以主机记录为键。
    // 连接失败时若是新建的则回滚，避免连不上的主机在侧栏堆积。
    let saved: Connection | null = null
    const created = !editing
    try {
      saved = await api.saveConnection({
        ID: connection?.ID ?? 0,
        Name: name.trim(),
        Host: host.trim(),
        Port: port,
        User: user.trim(),
        AuthMethod: authMethod,
        KeyPath: authMethod === 'key' ? keyPath.trim() : '',
        Cache: cacheMode === 'inherit' ? null : cacheMode === 'on',
        HasPassword: hasSaved,
        LastDir: connection?.LastDir ?? '',
        LastUsedAt: connection?.LastUsedAt ?? 0,
        Connected: false,
      })
      const cred: Credential = {
        Password: authMethod === 'password' ? secret : '',
        Passphrase: authMethod === 'key' ? secret : '',
      }
      await api.connectRemote(saved.ID, cred)
      message.success(`已连接到 ${saved.User}@${saved.Host}:${saved.Port}`)
      onConnected(saved)
    } catch (e) {
      if (created && saved) {
        await api.deleteConnection(saved.ID).catch(() => {})
      }
      message.error(`连接失败：${errText(e)}`)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Modal
      title={editing ? `编辑主机 · ${connection?.Name}` : '新建远端主机'}
      open={open}
      width={560}
      okText="连接"
      cancelText="取消"
      confirmLoading={loading}
      onOk={submit}
      onCancel={() => {
        if (!loading) onClose()
      }}
    >
      <Space orientation="vertical" size={10} style={{ width: '100%' }}>
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            主机地址
          </Typography.Text>
          <Space.Compact style={{ width: '100%' }}>
            <Input
              placeholder="IP 或域名，例如 10.0.0.5"
              value={host}
              onChange={(e) => setHost(e.target.value)}
              onPressEnter={submit}
            />
            <InputNumber
              style={{ width: 110 }}
              min={1}
              max={65535}
              value={port}
              onChange={(v) => setPort(Number(v) || 22)}
            />
          </Space.Compact>
        </div>

        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            用户名
          </Typography.Text>
          <Input
            placeholder="登录用户名，例如 root"
            value={user}
            onChange={(e) => setUser(e.target.value)}
            onPressEnter={submit}
          />
        </div>

        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            认证方式
          </Typography.Text>
          <Select
            style={{ width: '100%' }}
            value={authMethod}
            onChange={(v) => {
              setAuthMethod(v)
              setSecret('')
            }}
            options={AUTH_OPTIONS}
          />
        </div>

        {authMethod === 'key' && (
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              私钥文件（留空则自动探测 ~/.ssh/id_ed25519、id_rsa、id_ecdsa）
            </Typography.Text>
            <Space.Compact style={{ width: '100%' }}>
              <Input
                placeholder="~/.ssh/id_ed25519"
                value={keyPath}
                onChange={(e) => setKeyPath(e.target.value)}
              />
              <Button icon={<FolderOpenOutlined />} onClick={pickKey}>
                浏览
              </Button>
            </Space.Compact>
          </div>
        )}

        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {authMethod === 'password' ? '登录密码' : '私钥口令（私钥未加密则留空）'}
            {hasSaved ? '（已保存，留空即沿用）' : ''}
          </Typography.Text>
          <Input.Password
            placeholder={hasSaved ? '留空则使用已保存的口令' : '连接成功后保存到本机，下次免输入'}
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            onPressEnter={submit}
          />
          {hasSaved && connection && (
            <Typography.Text
              type="danger"
              style={{ fontSize: 12, cursor: 'pointer' }}
              onClick={() => {
                api
                  .clearConnectionSecret(connection.ID)
                  .then(async () => {
                    setHasSaved(false)
                    await refreshConnections().catch(() => {})
                    message.success('已清除保存的口令，下次连接需要重新输入')
                  })
                  .catch((e) => message.error(`清除失败：${errText(e)}`))
              }}
            >
              清除已保存的口令
            </Typography.Text>
          )}
        </div>

        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            本地缓存（远端文件内容缓存到本机，重新打开与翻页不必再走网络）
          </Typography.Text>
          <Select
            style={{ width: '100%' }}
            value={cacheMode}
            onChange={setCacheMode}
            options={CACHE_OPTIONS}
          />
        </div>

        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            备注名（可选）
          </Typography.Text>
          <Input
            placeholder="默认使用 用户名@主机"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          连接成功后，主机信息与口令会保存在本机数据库（明文），下次连接免输入；
          可在上方清除口令。启用缓存时，读过的日志内容也会缓存在本机（可在「本地缓存」中查看与清空）。
        </Typography.Text>
      </Space>
    </Modal>
  )
}
