package xorm

import (
	"errors"
	"reflect"
	"strconv"

	"github.com/xormplus/core"
)

// Get retrieve one record from database, bean's non-empty fields
// will be as conditions
func (session *Session) Get(bean interface{}) (bool, error) {
	defer session.resetStatement()
	if session.IsAutoClose {
		defer session.Close()
	}

	session.Statement.setRefValue(rValue(bean))

	var sqlStr string
	var args []interface{}

	if session.Statement.RawSQL == "" {
		if len(session.Statement.TableName()) <= 0 {
			return false, ErrTableNotFound
		}
		session.Statement.Limit(1)
		sqlStr, args = session.Statement.genGetSQL(bean)
	} else {
		sqlStr = session.Statement.RawSQL
		args = session.Statement.RawParams
	}

	if session.canCache() {
		if cacher := session.Engine.getCacher2(session.Statement.RefTable); cacher != nil &&
			!session.Statement.unscoped {
			has, err := session.cacheGet(bean, sqlStr, args...)
			if err != ErrCacheFailed {
				return has, err
			}
		}
	}

	return session.nocacheGet(bean, sqlStr, args...)
}

func (session *Session) nocacheGet(bean interface{}, sqlStr string, args ...interface{}) (bool, error) {
	var rawRows *core.Rows
	var err error
	session.queryPreprocess(&sqlStr, args...)
	if session.IsAutoCommit {
		_, rawRows, err = session.innerQuery(sqlStr, args...)
	} else {
		rawRows, err = session.Tx.Query(sqlStr, args...)
	}
	if err != nil {
		return false, err
	}

	defer rawRows.Close()

	if rawRows.Next() {
		if fields, err := rawRows.Columns(); err == nil {
			err = session.row2Bean(rawRows, fields, len(fields), bean)
		}
		return true, err
	}
	return false, nil
}

func (session *Session) cacheGet(bean interface{}, sqlStr string, args ...interface{}) (has bool, err error) {
	// if has no reftable, then don't use cache currently
	if !session.canCache() {
		return false, ErrCacheFailed
	}

	for _, filter := range session.Engine.dialect.Filters() {
		sqlStr = filter.Do(sqlStr, session.Engine.dialect, session.Statement.RefTable)
	}
	newsql := session.Statement.convertIDSQL(sqlStr)
	if newsql == "" {
		return false, ErrCacheFailed
	}

	cacher := session.Engine.getCacher2(session.Statement.RefTable)
	tableName := session.Statement.TableName()
	session.Engine.logger.Debug("[cacheGet] find sql:", newsql, args)
	ids, err := core.GetCacheSql(cacher, tableName, newsql, args)
	table := session.Statement.RefTable
	if err != nil {
		var res = make([]string, len(table.PrimaryKeys))
		rows, err := session.DB().Query(newsql, args...)
		if err != nil {
			return false, err
		}
		defer rows.Close()

		if rows.Next() {
			err = rows.ScanSlice(&res)
			if err != nil {
				return false, err
			}
		} else {
			return false, ErrCacheFailed
		}

		var pk core.PK = make([]interface{}, len(table.PrimaryKeys))
		for i, col := range table.PKColumns() {
			if col.SQLType.IsText() {
				pk[i] = res[i]
			} else if col.SQLType.IsNumeric() {
				n, err := strconv.ParseInt(res[i], 10, 64)
				if err != nil {
					return false, err
				}
				pk[i] = n
			} else {
				return false, errors.New("unsupported")
			}
		}

		ids = []core.PK{pk}
		session.Engine.logger.Debug("[cacheGet] cache ids:", newsql, ids)
		err = core.PutCacheSql(cacher, ids, tableName, newsql, args)
		if err != nil {
			return false, err
		}
	} else {
		session.Engine.logger.Debug("[cacheGet] cache hit sql:", newsql)
	}

	if len(ids) > 0 {
		structValue := reflect.Indirect(reflect.ValueOf(bean))
		id := ids[0]
		session.Engine.logger.Debug("[cacheGet] get bean:", tableName, id)
		sid, err := id.ToString()
		if err != nil {
			return false, err
		}
		cacheBean := cacher.GetBean(tableName, sid)
		if cacheBean == nil {
			/*newSession := session.Engine.NewSession()
			defer newSession.Close()
			cacheBean = reflect.New(structValue.Type()).Interface()
			newSession.Id(id).NoCache()
			if session.Statement.AltTableName != "" {
				newSession.Table(session.Statement.AltTableName)
			}
			if !session.Statement.UseCascade {
				newSession.NoCascade()
			}
			has, err = newSession.Get(cacheBean)
			*/
			cacheBean = bean
			has, err = session.nocacheGet(cacheBean, sqlStr, args...)
			if err != nil || !has {
				return has, err
			}

			session.Engine.logger.Debug("[cacheGet] cache bean:", tableName, id, cacheBean)
			cacher.PutBean(tableName, sid, cacheBean)
		} else {
			session.Engine.logger.Debug("[cacheGet] cache hit bean:", tableName, id, cacheBean)
			has = true
		}
		structValue.Set(reflect.Indirect(reflect.ValueOf(cacheBean)))

		return has, nil
	}
	return false, nil
}
