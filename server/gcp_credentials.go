package main

// GCP Service Account 凭证的存储层：多份并存（多账号），每台节点绑定其中一份。
// 上层编排见 gcp_autostart.go，HTTP 接口见 gcp.go。

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// gcpCredential 一份 Service Account 凭证。
//
// SaJSON 是密文，且是全库最敏感的一个字段——它能开关用户的机器。
// 任何走到 HTTP 响应的路径都必须先转成 gcpCredentialView，不要图省事直接序列化本结构体。
type gcpCredential struct {
	ID          string
	ProjectID   string
	ClientEmail string
	SaJSON      string
	CreatedAt   int64
	ServerCount int // 仅 listGCPCredentials 填充：绑定本凭证且已开启自动开机的节点数
}

var (
	// errGCPNoCredential 一份凭证都没有。与「有凭证但没绑」是两回事，处置也不同：
	// 前者去添加凭证，后者去节点上选一份。
	errGCPNoCredential = errors.New("未配置 Service Account 凭证，请先到「GCP 守护」页添加")
	// errGCPCredAmbiguous 存了多份凭证，但这台节点没说用哪份。
	errGCPCredAmbiguous = errors.New("面板中存有多份 GCP 凭证，请在服务器编辑里指定这台节点使用哪一份")
)

/* ---------- 老库迁移 ---------- */

// migrateGCPCredentials 把多凭证之前的全局单份凭证（settings.gcp_sa_json）
// 搬进 gcp_credentials 表，并把所有未绑定的节点绑到它上面。
//
// 必须在 initSecret 之后调用，不能塞进 openDB：main() 里 openDB 早于 initSecret，
// 那时主密钥还是全零，解密必然失败，迁移会在每次启动时静默失败到天荒地老，
// 而失败现象与「用户真的弄丢了主密钥」完全一样，根本查不出来。
//
// 返回 error 仅表示「这次没做成、下次启动重试」，调用方记日志即可，不要 Fatal——
// GCP 自动开机是附加功能，它迁移不了不该拖着整个监控面板不启动。
func migrateGCPCredentials(db *sql.DB) error {
	if getSetting(db, keyGCPCredMigrated, "") != "" {
		return nil
	}
	stored := strings.TrimSpace(getSetting(db, keyGCPSAJSON, ""))
	if stored == "" {
		return markGCPCredMigrated(db)
	}

	plain, err := decryptSecretValue(stored)
	if err != nil {
		// 不写哨兵：这是可恢复的。用户把原来的 MOSS_SECRET_KEY / secret.key 找回来，
		// 下次启动迁移自动补做。写了哨兵就等于替用户宣布这份凭证永久作废。
		log.Printf("GCP 凭证迁移暂缓：旧凭证解密失败（%v）。恢复原 MOSS_SECRET_KEY 或 secret.key 后重启面板会自动完成迁移；在此之前 GCP 自动开机不可用", err)
		return err
	}
	if plain = strings.TrimSpace(plain); plain == "" {
		return markGCPCredMigrated(db)
	}

	sa, _, err := parseGCPSA(plain)
	if err != nil {
		// 与上面相反：内容坏了不会自己变好，卡着不迁只会让哨兵永远悬着，
		// 每次启动重复解析同一份坏数据。标记完成，让用户去重新添加。
		log.Printf("GCP 凭证迁移跳过：旧凭证内容无法解析（%v）。请到「GCP 守护」页重新添加凭证", err)
		return markGCPCredMigrated(db)
	}

	// 落库值：已是密文就原样搬。encryptSecret 非幂等，重加密会套成两层，
	// 之后再也解不出原文。
	//
	// 无前缀说明是加密落地之前的老明文（decryptSecretValue 对它原样透传）。
	// 它必须在这里补加密：secret.go 里「下次写入时自动升级为密文」的那条升级路径
	// 依赖的是有人再写一次 gcp_sa_json，而搬进新表后这个键就再没人写了——
	// 不补加密，这份明文私钥就在库里躺到永远。
	toStore := stored
	if !strings.HasPrefix(stored, encPrefix) {
		if toStore, err = encryptSecret(plain); err != nil {
			// 同样不写哨兵：加密不成宁可下次重试，也不能把明文私钥搬进新表。
			log.Printf("GCP 凭证迁移暂缓：加密失败（%v），下次启动重试", err)
			return err
		}
	}

	// 迁移失败过（解密失败、无哨兵）而用户已经在界面上手工补加了同一份凭证时，
	// 直接 INSERT 会撞上 client_email 唯一索引。复用已有那条，只补绑定。
	var id string
	switch err := db.QueryRow(
		`SELECT id FROM gcp_credentials WHERE client_email = ?`, sa.ClientEmail).Scan(&id); {
	case err == nil:
		log.Printf("GCP 凭证迁移：%s 已在凭证列表中，仅补做节点绑定", sa.ClientEmail)
	case errors.Is(err, sql.ErrNoRows):
		id = ""
	default:
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if id == "" {
		id = randString(8)
		if _, err := tx.Exec(
			`INSERT INTO gcp_credentials(id, project_id, client_email, sa_json, created_at) VALUES(?, ?, ?, ?, ?)`,
			id, sa.ProjectID, sa.ClientEmail, toStore, time.Now().Unix()); err != nil {
			return err
		}
	}
	// 不限 gcp_enabled = 1：把当前关着开关的节点也预绑上，用户回头打开开关就能直接用。
	res, err := tx.Exec(`UPDATE servers SET gcp_cred_id = ? WHERE gcp_cred_id = ''`, id)
	if err != nil {
		return err
	}
	// setSetting 只接受 *sql.DB，事务里内联同样的 upsert，保证「插凭证 + 绑节点 + 置哨兵」
	// 三件事原子完成。否则崩在中间会在下次启动重复插入。
	if _, err := tx.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		keyGCPCredMigrated, "1"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bound, _ := res.RowsAffected()
	log.Printf("GCP 凭证已迁移为多凭证结构：%s（项目 %s），绑定 %d 台节点", sa.ClientEmail, sa.ProjectID, bound)
	return nil
}

func markGCPCredMigrated(db *sql.DB) error {
	return setSetting(db, keyGCPCredMigrated, "1")
}

/* ---------- 读写 ---------- */

func loadGCPCredential(db *sql.DB, id string) (*gcpCredential, error) {
	var c gcpCredential
	err := db.QueryRow(
		`SELECT id, project_id, client_email, sa_json, created_at FROM gcp_credentials WHERE id = ?`, id).
		Scan(&c.ID, &c.ProjectID, &c.ClientEmail, &c.SaJSON, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("凭证 %s 不存在（可能已被删除），请在服务器编辑里重新选择", id)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// listGCPCredentials 列出全部凭证。ServerCount 只数「已开启自动开机」的节点，
// 与删除时的占用判定用同一口径——列表上显示 0 台在用，就一定删得掉。
//
// 次级排序用 rowid 而不是 id：created_at 只到秒，同一秒内添加两份时按随机的 id
// 排会让先加的那份跑到后面。rowid 就是插入顺序，同秒也稳。
func listGCPCredentials(db *sql.DB) ([]gcpCredential, error) {
	rows, err := db.Query(
		`SELECT c.id, c.project_id, c.client_email, c.sa_json, c.created_at,
		        (SELECT COUNT(*) FROM servers s WHERE s.gcp_cred_id = c.id AND s.gcp_enabled = 1)
		 FROM gcp_credentials c ORDER BY c.created_at, c.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []gcpCredential{}
	for rows.Next() {
		var c gcpCredential
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ClientEmail, &c.SaJSON, &c.CreatedAt, &c.ServerCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func countGCPCredentials(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM gcp_credentials`).Scan(&n)
	return n, err
}

// gcpCredCountAndOnly 一次查出凭证总数，以及「恰好一份」时那份的 id。
// MIN(id) 在只有一行时就是该行的 id；多行时调用方只看 n，不用 only。
func gcpCredCountAndOnly(db *sql.DB) (n int, only string, err error) {
	err = db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(id), '') FROM gcp_credentials`).Scan(&n, &only)
	return
}

// gcpOnlyCredentialID 恰好只有一份凭证时返回它的 id，否则返回空串。
func gcpOnlyCredentialID(db *sql.DB) string {
	n, only, err := gcpCredCountAndOnly(db)
	if err != nil || n != 1 {
		return ""
	}
	return only
}

// resolveGCPCredID 定出一台节点该用哪份凭证。
//
// 绑定 id 存在但查无此凭证时一律报错，绝不「反正只有一份就用那份」地兜底：
// 那会把一次静默的数据损坏伪装成正常工作，而猜错的代价是拿 A 账号的凭证
// 去开 B 账号的机器。悬空只可能来自残缺的备份恢复或手改数据库，本就该显式暴露。
func resolveGCPCredID(db *sql.DB, credID string) (string, error) {
	if credID = strings.TrimSpace(credID); credID != "" {
		var got string
		err := db.QueryRow(`SELECT id FROM gcp_credentials WHERE id = ?`, credID).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("绑定的凭证 %s 已不存在，请在服务器编辑里重新选择凭证", credID)
		}
		if err != nil {
			return "", err
		}
		return got, nil
	}
	// 未绑定：只有一份凭证时不为难用户，直接用它。多于一份就必须显式指定——
	// 写入侧（新增凭证、编辑节点）已经在堵这个状态，走到这里说明是手改过的库。
	n, only, err := gcpCredCountAndOnly(db)
	if err != nil {
		return "", err
	}
	switch {
	case n == 0:
		return "", errGCPNoCredential
	case n > 1:
		return "", errGCPCredAmbiguous
	}
	return only, nil
}
