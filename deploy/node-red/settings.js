// Node-RED 运行配置。只写与默认值不同的项，其余走官方默认。
//
// 无状态：刻意不配置 contextStorage，流程不承载跨消息状态（详细设计 §4.5）。
// 去抖计数与活动告警集合都在接入服务内存里，编排层只做管道。

module.exports = {
    flowFile: 'flows.json',
    uiPort: process.env.PORT || 1880,

    // 导出给版本控制的 flows.json 按多行缩进写，diff 才看得懂。
    flowFilePretty: true,

    // 流程里没有任何 Node-RED 凭据：数据库账号口令都走环境变量（userFieldType=env）。
    // 关掉加密就不会在每次启动时生成随机密钥并刷一段警告。
    credentialSecret: false,

    logging: {
        console: {
            level: 'info',
            metrics: false,
            audit: false,
        },
    },
    editorTheme: {
        projects: {
            enabled: false,
        },
    },
    functionExternalModules: false,
};