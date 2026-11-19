# HTTP契约

所有写入场景的JSON入口拒绝未知字段和尾随内容，最大8MiB。服务返回JSON错误；404为对象不存在，409为版本或生命周期冲突，429为成像容量不足，400为不符合输入契约，504为超时。未知路由由HTTP框架返回404。

| 方法与路径 | 输入及结果 |
|---|---|
| GET /healthz | ready |
| POST /v1/plates | id,title,document →201版面 |
| GET /v1/plates/{id} | 完整稿本历史 |
| PUT /v1/plates/{id} | expected_revision,document →追加稿本 |
| POST /v1/plates/{id}/branch | revision,id,title →201独立版面 |
| POST /v1/plates/{id}/archive | 禁止修订；历史仍可读取 |
| POST /v1/plates/{id}/render?revision=1&format=png | 201成品元数据；format也可为svg |
| POST /v1/plates/{id}/hit?revision=1 | 画布点拾取：排序候选、遮挡/镂空/裁域/透明解释 |
| POST /v1/plates/{id}/exposures?revision=1&format=png | 202瞬态任务 |
| GET /v1/exposures/{id} | queued/running/succeeded/failed/cancelled |
| DELETE /v1/exposures/{id} | 202取消请求 |
| GET /v1/artifacts/{id} | 图片；ETag为成品字节摘要；X-Scene-Digest为稿本摘要 |
| DELETE /v1/artifacts/{id} | 撤销成品 |
| POST /v1/proofs | id,artifact →201 open校样 |
| PUT /v1/proofs/{id} | state为accepted或rejected，note为说明 |
| GET /v1/plates/{id}/bundle | 含完整稿本历史的封包JSON |
| POST /v1/bundles | 原样封包；201导入，重名409 |

成品响应中的data为null，避免重复传输二进制；GET成品获得实际内容。If-None-Match精确匹配强ETag时返回304。成功校样可从持久化快照确认，当前版本没有校样列表/详情接口。GET版面返回完整历史，服务没有全局列表接口。

## 画布拾取

`POST /v1/plates/{id}/hit?revision=N` 接收 `{"x":…,"y":…,"options":{…}}`，坐标原点在左上，单位是版面像素。它针对指定稿本重新走同一编译准入，再用与 PNG 相同的合成函数评估该点，不读取成品缓存，也不依赖最上层包围盒。

```json
{"x":30,"y":70,"options":{"min_contribution":0.0005,"edge_tolerance_px":1.5}}
```

结果字段：

| 字段 | 含义 |
|---|---|
| selected | 排序中第一个可选候选；无可选时为缺省 |
| candidates | 局部相关形状，按 rank 升序；不可选者保留为证据 |
| ambiguous | 归因不唯一时的有序形状 id；仍给出稳定排序，但声明不唯一 |
| options | 解析后的容差与权重，逐次回显以便复现同一排序 |
| note | 空点、边缘拾取、微弱贡献、联合效果的说明 |

每个候选分别给出：几何（inside/hole/edge/edge_distance_px/local_point/transform/fill_rule）、笔刷（brush/local_alpha/layer_opacity）、合成证据（contribution/occlusion/occluded_by/layer_gate_at_point/layer_clip）和排序依据（components/score/reasons）。`occlusion` 取值 visible、faint、covered、transparent、hole、outside、clipped、masked。

`contribution` 是把该形状从所在图层单独移除后，该点预乘线性光 RGB 的平均变化；镂空、图层与形状叠加、组透明度、multiply/screen 都已包含在内，不做特判。实现上按源叠加的前缀/后缀折叠计算，与整幅重新合成等价（逐点核对误差 ≤5e-7），单点开销随形状数线性增长而非平方。

### 可调项

| 选项 | 默认 | 作用 |
|---|---|---|
| edge_tolerance_px | 1.5 | 填充外但在展平轮廓该距离内，仍可作为边缘候选；0..64，显式 0 关闭边缘拾取 |
| min_contribution | 0.0005 | 可选中所需的可见变化下限；低于它仍报告为 faint 证据，显式 0 接受任何可测变化 |
| ambiguity_margin | 0.06 | 分数差在此内即声明归因不唯一 |
| joint_share | 0.75 | 首选形状解释的像素变化占画面总变化的最低比例；低于它声明联合效果 |
| max_candidates | 8 | 证据条数上限，1..32 |
| include_covered | false | 显式穿透选择被后绘制的画笔完全覆盖的形状 |
| weights | 见下 | coverage 0.40、contribution 0.25、opacity 0.15、topmost 0.20，按和归一 |

省略字段取默认，显式 0 表示真正的 0；因此 `edge_tolerance_px:0` 关闭边缘拾取，`min_contribution:0` 接受任何可测变化。`options` 在响应中回显的是补齐后的生效值。

排序语义：先选可选择性，再按加权分，再按绘制顺序（后绘制者优先）作稳定决胜。topmost 只是弱先验，权重可调到 0；空洞、裁域和透明区即便位于最上层也不会因顺序取胜。`weights` 可改数值，不能改含义。

### 边界与不可唯一归因

- 边缘容差以版面像素计，距离按仿射变换后的展平折线计算；Q/C 的固定细分与外扩因此与 PNG/SVG 使用同一几何。低于约 1px 的极细描边可能落在容差带内，属于明确取舍。
- 低透明物体默认可选：`min_contribution` 默认极小，5% 白色罩层贡献 0.043，仍可选中；若只想选择明显可见的对象，调高该值，该候选变为 `faint` 且 `selectable=false`，但仍在证据列表中，不会被静默丢弃。
- 被不透明后绘制画笔完全覆盖的形状记为 `covered`，默认不可选；`include_covered=true` 才允许穿透。`occluded_by` 列出实际覆盖该点的形状。
- 当首选形状只解释部分像素变化（低于 `joint_share`）或分数差在 `ambiguity_margin` 内，`ambiguous` 返回联合归因的有序列表。服务不声称像素由单一对象拥有。
- 画布外、非有限坐标与越界选项返回 400；未知稿本或稿本号返回 404。

真实 PNG 证据由 `python3 scripts/selection_evidence.py --out <dir>` 生成：它实际请求 PNG，用标准库解码并逐像素核对镂空、裁域、低透明、渐变两端与遮挡，同时写出标注图与 evidence.json。`python3 scripts/walkthrough.py --workflow selection` 通过 HTTP 检查同一组语义。

仓库内已有一次运行的产物：

| 文件 | 内容 |
|---|---|
| docs/evidence/selection-proof.png | 该场景的权威 PNG（160×120） |
| docs/evidence/selection-proof-annotated.png | 同一 PNG 叠加 7 个核对点标记 |
| docs/evidence/evidence.json | 各点命中结果与对应 PNG 像素值 |
| docs/evidence/selection-proof.json | 生成上述内容的场景副本 |

标注点与像素的对应关系：

| 点 | 坐标 | 选择结果 | PNG 像素 | 说明 |
|---|---|---|---|---|
| hole | 65,25 | under-plate | 38,89,153 | 环形镂空内露出底色，环本身不可选 |
| ring | 45,25 | ring | 51,191,179 | 环形实体带被选中 |
| clip | 115,60 | 无 | 13,15,20 | 填充覆盖但被图层裁域排除，等同背景 |
| cover | 60,60 | cover-block | 243,149,74 | 后绘制的不透明块覆盖下方形状 |
| veil | 30,70 | under-plate（联合） | 74,106,160 | 5% 罩层可见并可选中，但归因不唯一 |
| gradient | 135,105 | fade-bar | 226,50,150 | 渐变不透明端，候选贡献最大 |
| empty | 100,35 | 无 | 13,15,20 | 空白区域，无候选 |

这些数值由脚本断言；人工查看标注图可确认标记落在预期语义区域。
