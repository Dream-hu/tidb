# TiDB HTTP API

`TiDBIP` is the ip of the TiDB server. `10080` is the default status port, and you can edit it in tidb.toml when starting the TiDB server.

1. Get the current status of TiDB, including the connections, version and git_hash

    ```shell
    curl http://{TiDBIP}:10080/status
    ```

    ```shell
    $curl http://127.0.0.1:10080/status
    {
        "connections": 0,
        "git_hash": "f572e33854e1c0f942f031e9656d0004f99995c6",
        "version": "5.7.25-TiDB-v2.1.0-rc.3-355-gf572e3385-dirty",
        "status":{
          "init_stats_percentage":100
        }
    }
    ```

2. Get all metrics of TiDB

    ```shell
    curl http://{TiDBIP}:10080/metrics
    ```

3. Get the metadata of all regions

    ```shell
    curl http://{TiDBIP}:10080/regions/meta
    ```

    ```shell
    $curl http://127.0.0.1:10080/regions/meta
    [
        {
            "leader": {
                "id": 5,
                "store_id": 1
            },
            "peers": [
                {
                    "id": 5,
                    "store_id": 1
                }
            ],
            "region_epoch": {
                "conf_ver": 1,
                "version": 2
            },
            "region_id": 4
        }
    ]
    ```

4. Get the table/index of hot regions

    ```shell
    curl http://{TiDBIP}:10080/regions/hot
    ```

    ```shell
    $curl http://127.0.0.1:10080/regions/hot
    {
      "read": [

      ],
      "write": [
        {
          "db_name": "sbtest1",
          "table_name": "sbtest13",
          "index_name": "",
          "flow_bytes": 220718,
          "max_hot_degree": 12,
          "region_count": 1
        }
      ]
    }
    ```

5. Get the information of a specific region by ID

    ```shell
    curl http://{TiDBIP}:10080/regions/{regionID}
    ```

    ```shell
    $curl http://127.0.0.1:10080/regions/4001
    {
        "end_key": "dIAAAAAAAAEk",
        "frames": [
            {
                "db_name": "test",
                "is_record": true,
                "table_id": 286,
                "table_name": "t1"
            }
        ],
        "region_id": 4001,
        "start_key": "dIAAAAAAAAEe"
    }
    ```

6. Get regions Information from db.table

    ```shell
    curl http://{TiDBIP}:10080/tables/{db}/{table}/regions
    ```

    ```shell
    $curl http://127.0.0.1:10080/tables/test/t1/regions
    {
        "id": 286,
        "indices": [],
        "name": "t1",
        "record_regions": [
            {
                "leader": {
                    "id": 4002,
                    "store_id": 1
                },
                "peers": [
                    {
                        "id": 4002,
                        "store_id": 1
                    }
                ],
                "region_epoch": {
                    "conf_ver": 1,
                    "version": 83
                },
                "region_id": 4001
            }
        ]
    }
    ```

7. Get schema Information about all db

    ```shell
    curl http://{TiDBIP}:10080/schema
    ```

    ```shell
    $curl http://127.0.0.1:10080/schema
    [
        {
            "charset": "utf8mb4",
            "collate": "utf8mb4_bin",
            "db_name": {
                "L": "test",
                "O": "test"
            },
            "id": 266,
            "state": 5
        },
        .
        .
        .
    ]
    ```

8. Get schema Information about db

    ```shell
    curl http://{TiDBIP}:10080/schema/{db}
    ```

    ```shell
    curl http://{TiDBIP}:10080/schema/{db}?id_name_only=true
    [
    {
     "id": 119,
     "name": {
      "O": "t1",
      "L": "t1"
     }
    },
    {
     "id": 125,
     "name": {
      "O": "t2",
      "L": "t2"
     }
    }
    ]
    ```

9. Get schema Information about db.table, and you can get schema info by tableID (tableID is the **unique** identifier of table in TiDB)

    ```shell
    curl http://{TiDBIP}:10080/schema/{db}/{table}

    curl http://{TiDBIP}:10080/schema?table_id={tableID}

    curl http://{TiDBIP}:10080/schema?table_ids={tableID,...}
    ```

10. Get database information, table information and tidb info schema version by tableID.

     ```shell
     curl http://{TiDBIP}:10080/db-table/{tableID}
     ```

11. Get MVCC Information of the key with a specified handle ID

     ```shell
     curl http://{TiDBIP}:10080/mvcc/key/{db}/{table}/{handle}
     ```

     ```shell
     $curl http://127.0.0.1:10080/mvcc/key/test/t/1
     {
         "key": "74800000000000006E5F728000000000000001",
         "region_id": 4,
         "value": {
             "info": {
                 "writes": [
                     {
                         "start_ts": 448662063415296001,
                         "commit_ts": 448662063415296003,
                         "short_value": "gAABAAAAAQEAAQ=="
                     }
                 ]
             }
         }
     }
     ```

     If the handle is clustered, specify the primary key column values in the query string

     ```shell
     $curl "http://{TiDBIP}:10080/mvcc/key/{db}/{table}?${c1}={v1}&${c2}=${v2}"
     ```

     ```shell
     $curl "http://127.0.0.1:10080/mvcc/key/test/t?a=aaa&b=2020-01-01"
     {
         "key": "7480000000000000365F72016161610000000000FA0419A5420000000000",
         "region_id": 52,
         "value": {
             "info": {
                 "writes": [
                     {
                         "type": 1,
                         "start_ts": 423158426542538752,
                         "commit_ts": 423158426543587328
                     },
                     {
                         "start_ts": 423158426542538752,
                         "commit_ts": 423158426543587328,
                         "short_value": "gAACAAAAAQMDAAQAYWFhZA=="
                     }
                 ],
                 "values": [
                     {
                         "start_ts": 423158426542538752,
                         "value": "gAACAAAAAQMDAAQAYWFhZA=="
                     }
                 ]
             }
         }
     }
     ```

     *Hint: The meaning of the MVCC operation type:*

     ```protobuf
     enum Op {
         Put = 0;
         Del = 1;
         Lock = 2;
         Rollback = 3;
         // insert operation has a constraint that key should not exist before.
         Insert = 4;
         PessimisticLock = 5;
         CheckNotExists = 6;
     }
     ```

     *Hint: On a partitioned table, use the `table(partition)` pattern as the table name, `t1(p1)` for example:*

     ```shell
     $curl http://127.0.0.1:10080/mvcc/key/test/t1(p1)/1
     ```

     *Hint: The method to convert the Hex format key returned by TiDB API into the format recognized by [tikv-ctl](https://docs.pingcap.com/tidb/stable/tikv-control).*

     Step 1: Get the hex format of the key you need. For example you could find the key by the following TiDB API.

      ```shell
         $curl http://127.0.0.1:10080/mvcc/key/test/t1/1
         {
         "key": "7480000000000008C65F728000000000000001",
         "region_id": 10,
         "value": {
             "info": {
                 "writes": [
                     {
                         "start_ts": 445971968923271174,
                         "commit_ts": 445971968923271175,
                         "short_value": "gAACAAAAAgMIAAkAc2hpcmx5YTQi"
                     },
                     {
                         "start_ts": 445971959499980803,
                         "commit_ts": 445971959499980804,
                         "short_value": "gAACAAAAAgMIAAkAc2hpcmx5YTQL"
                     }
                 ]
             }
         }
         }
      ```

      Step 2: Convert the key from hex format to escaped format with [tikv-ctl](https://docs.pingcap.com/tidb/stable/tikv-control)

      ```shell
       ./tikv-ctl --to-escaped '7480000000000008C65F728000000000000001'
       t\200\000\000\000\000\000\010\306_r\200\000\000\000\000\000\000\001
      ```

      Step 3: Encode the key to make it memcomparable in tikv with [tikv-ctl](https://docs.pingcap.com/tidb/stable/tikv-control)

      ```shell
      ./tikv-ctl --encode 't\200\000\000\000\000\000\010\306_r\200\000\000\000\000\000\000\001'
      7480000000000008FFC65F728000000000FF0000010000000000FA
      ```

      Step 4: Convert the key from hex format to escaped format again since most `tikv-ctl` commands only accept keys in escaped format while the `--encode` command outputs the key in hex format.

      ```shell
      ./tikv-ctl --to-escaped '7480000000000008FFC65F728000000000FF0000010000000000FA'
      t\200\000\000\000\000\000\010\377\306_r\200\000\000\000\000\377\000\000\001\000\000\000\000\000\372
      ```

      Step 5: Add a prefix "z" to the key. Then the key can be recognized by [tikv-ctl](https://docs.pingcap.com/tidb/stable/tikv-control). For example, use the following command to scan from tikv.

      ```shell
      ./tikv-ctl  --host "<tikv_ip>:<port>" scan --from 'zt\200\000\000\000\000\000\010\377\306_r\200\000\000\000\000\377\000\000\001\000\000\000\000\000\372' --limit 5 --show-cf write,lock,default 
         key: zt\200\000\000\000\000\000\010\377\306_r\200\000\000\000\000\377\000\000\001\000\000\000\000\000\372
          write cf value: start_ts: 445971968923271174 commit_ts: 445971968923271175 short_value: 800002000000020308000900736869726C79613422
          write cf value: start_ts: 445971959499980803 commit_ts: 445971959499980804 short_value: 800002000000020308000900736869726C7961340B

         key: zt\200\000\000\000\000\000\010\377\306_r\200\000\000\000\000\377\000\000\002\000\000\000\000\000\372
          write cf value: start_ts: 445971960836390913 commit_ts: 445971960836390914 short_value: 80000200000002030500060073686972340B

         key: zt\200\000\377\377\377\377\377\377\373_r\200\000\000\000\000\377\000\000\003\000\000\000\000\000\372
          write cf value: r_type: Del start_ts: 444068474890485761 commit_ts: 444068474890485762

         key: zt\200\000\377\377\377\377\377\377\373_r\200\000\000\000\000\377\000\000\005\000\000\000\000\000\372
          write cf value: r_type: Del start_ts: 444068474929545217 commit_ts: 444068474929545218

         key: zt\200\000\377\377\377\377\377\377\373_r\200\000\000\000\000\377\000\000\007\000\000\000\000\000\372
          write cf value: r_type: Del start_ts: 444068474981974017 commit_ts: 444068474981974018
      ```

12. Get MVCC Information of the first key in the table with a specified start ts

     ```shell
     curl http://{TiDBIP}:10080/mvcc/txn/{startTS}/{db}/{table}
     ```

     ```shell
     $curl http://127.0.0.1:10080/mvcc/txn/405179368526053377/test/t1
     {
         "info": {
             "writes": [
                 {
                     "commit_ts": 405179368526053380,
                     "short_value": "CAICAkE=",
                     "start_ts": 405179368526053377
                 }
             ]
         },
         "key": "dIAAAAAAAAEzX3KAAAAAAAAAAQ=="
     }
     ```

13. Get MVCC Information by a hex value

     ```shell
     curl http://{TiDBIP}:10080/mvcc/hex/{hexKey}
     ```

14. Get MVCC Information of a specified index key, argument example: column_name_1=column_value_1&column_name_2=column_value2...

     ```shell
     curl "http://{TiDBIP}:10080/mvcc/index/{db}/{table}/{index}/{handle}?${c1}={v1}&${c2}=${v2}"
     ```

     *Hint: For the index column which column type is timezone dependent, e.g. `timestamp`, convert its value to UTC
timezone.*

     ```shell
     $curl "http://127.0.0.1:10080/mvcc/index/test/t1/idx/1?a=A"
     {
         "info": {
             "writes": [
                 {
                     "commit_ts": 405179523374252037,
                     "short_value": "MA==",
                     "start_ts": 405179523374252036
                 }
             ]
         }
     }
     ```

     *Hint: On a partitioned table, use the `table(partition)` pattern as the table name, `t1(p1)` for example:*

     ```shell
     $curl "http://127.0.0.1:10080/mvcc/index/test/t1(p1)/idx/1?a=A"
     ```

    If the handle is clustered, also specify the primary key column values in the query string

    ```shell
    $curl "http://{TiDBIP}:10080/mvcc/index/{db}/{table}/{index}?${c1}={v1}&${c2}=${v2}"
    ```

    ```shell
    $curl "http://127.0.0.1:10080/mvcc/index/test/t/idx?a=1.1&b=111&c=1"
    {
        "key": "74800000000000003B5F69800000000000000203800000000000000105BFF199999999999A013131310000000000FA",
        "region_id": 59,
        "value": {
            "info": {
                "writes": [
                    {
                        "start_ts": 424752858505150464,
                        "commit_ts": 424752858506461184,
                        "short_value": "AH0B"
                    }
                ],
                "values": [
                    {
                         "start_ts": 424752858505150464,
                         "value": "AH0B"
                    }
                ]
            }
        }
    }

15. Scatter regions of the specified table, add a `scatter-range` scheduler for the PD and the range is same as the table range.

     ```shell
     curl http://{TiDBIP}:10080/tables/{db}/{table}/scatter
     ```

     *Hint: On a partitioned table, use the `table(partition)` pattern as the table name, `test(p1)` for example.*

     **Note**: The `scatter-range` scheduler may conflict with the global scheduler, do not use it for long periods on the larger table.

16. Stop scatter the regions, disable the `scatter-range` scheduler for the specified table.

     ```shell
     curl http://{TiDBIP}:10080/tables/{db}/{table}/stop-scatter
     ```

     *Hint: On a partitioned table, use the `table(partition)` pattern as the table name, `test(p1)` for example.*

17. Get TiDB server settings

     ```shell
     curl http://{TiDBIP}:10080/settings
     ```

18. Get TiDB server information.

     ```shell
     curl http://{TiDBIP}:10080/info
     ```

     ```shell
     $curl http://127.0.0.1:10080/info
     {
         "ddl_id": "f7e73ed5-63b4-4cb4-ba7c-42b32dc74e77",
         "git_hash": "f572e33854e1c0f942f031e9656d0004f99995c6",
         "ip": "",
         "is_owner": true,
         "lease": "45s",
         "listening_port": 4000,
         "status_port": 10080,
         "version": "5.7.25-TiDB-v2.1.0-rc.3-355-gf572e3385-dirty"
     }
     ```

19. Get TiDB cluster all servers information.

     ```shell
     curl http://{TiDBIP}:10080/info/all
     ```

     ```shell
     $curl http://127.0.0.1:10080/info/all
     {
         "servers_num": 2,
         "owner_id": "29a65ec0-d931-4f9e-a212-338eaeffab96",
         "is_all_server_version_consistent": true,
         "all_servers_info": {
             "29a65ec0-d931-4f9e-a212-338eaeffab96": {
                 "version": "5.7.25-TiDB-v4.0.0-alpha-669-g8f2a09a52-dirty",
                 "git_hash": "8f2a09a52fdcaf9d9bfd775d2c6023f363dc121e",
                 "ddl_id": "29a65ec0-d931-4f9e-a212-338eaeffab96",
                 "ip": "",
                 "listening_port": 4000,
                 "status_port": 10080,
                 "lease": "45s",
                 "binlog_status": "Off"
             },
             "cd13c9eb-c3ee-4887-af9b-e64f3162d92c": {
                 "version": "5.7.25-TiDB-v4.0.0-alpha-669-g8f2a09a52-dirty",
                 "git_hash": "8f2a09a52fdcaf9d9bfd775d2c6023f363dc121e",
                 "ddl_id": "cd13c9eb-c3ee-4887-af9b-e64f3162d92c",
                 "ip": "",
                 "listening_port": 4001,
                 "status_port": 10081,
                 "lease": "45s",
                 "binlog_status": "Off"
             }
         }
     }
     ```

20. Enable/Disable TiDB server general log

     ```shell
     curl -X POST -d "tidb_general_log=1" http://{TiDBIP}:10080/settings
     curl -X POST -d "tidb_general_log=0" http://{TiDBIP}:10080/settings
     ```

21. Change TiDB server log level

     ```shell
     curl -X POST -d "log_level=debug" http://{TiDBIP}:10080/settings
     curl -X POST -d "log_level=info" http://{TiDBIP}:10080/settings
     ```

22. Change TiDB DDL slow log threshold

     The unit is millisecond.

     ```shell
     curl -X POST -d "ddl_slow_threshold=300" http://{TiDBIP}:10080/settings
     ```

23. Get the column value by an encoded row and some information that can be obtained from a column of the table schema information. 

     Argument example: rowBin=base64_encoded_row_value

     ```shell
     curl http://{TiDBIP}:10080/tables/{colID}/{colFlag}/{colLen}?rowBin={val}
     ```

     *Hint: For the column which field type is timezone dependent, e.g. `timestamp`, convert its value to UTC timezone.*

24. Resign the ddl owner, let tidb start a new ddl owner election.

     ```shell
     curl -X POST http://{TiDBIP}:10080/ddl/owner/resign
     ```

    **Note**: If you request a TiDB that is not ddl owner, the response will be `This node is not a ddl owner, can't be resigned.`

25. Get the TiDB DDL job history information.

     ```shell
     curl http://{TiDBIP}:10080/ddl/history
     ```

     **Note**: When the DDL history is very very long, system table may containg too many jobs. This interface will get a maximum of 2048 history ddl jobs by default. If you want get more jobs, consider adding `start_job_id` and `limit`.

26. Get count {number} TiDB DDL job history information.

     ```shell
     curl http://{TiDBIP}:10080/ddl/history?limit={number}
     ```

27. Get count {number} TiDB DDL job history information, start with job {id}

     ```shell
     curl "http://{TIDBIP}:10080/ddl/history?start_job_id={id}&limit={number}"
     ```

28. Download TiDB debug info

     ```shell
     curl http://{TiDBIP}:10080/debug/zip?seconds=60 --output debug.zip
     ```

     zip file will include:

     - Go heap pprof(after GC)
     - Go cpu pprof(10s)
     - Go mutex pprof
     - Full goroutine
     - TiDB config and version

     Param:

     - seconds: profile time(s), default is 10s. 

29. Get statistics data of specified table.

     ```shell
     curl http://{TiDBIP}:10080/stats/dump/{db}/{table}
     ```

30. Get statistics data of specific table and timestamp.

     ```shell
     curl http://{TiDBIP}:10080/stats/dump/{db}/{table}/{yyyyMMddHHmmss}
     ```

     ```shell
     curl http://{TiDBIP}:10080/stats/dump/{db}/{table}/{yyyy-MM-dd HH:mm:ss}
     ```

31. Resume the binlog writing when Pump is recovered.

     ```shell
     curl http://{TiDBIP}:10080/binlog/recover
     ```

     Return value:

     - timeout, return status code: 400, message: `timeout`
     - If it returns normally, status code: 200, message example:

         ```text
         {
           "Skipped": false,
           "SkippedCommitterCounter": 0
         }
         ```

         `Skipped`: false indicates that the current binlog is not in the skipped state, otherwise, it is in the skipped state
         `SkippedCommitterCounter`: Represents how many transactions are currently being committed in the skipped state. By default, the API will return after waiting until all skipped-binlog transactions are committed. If this value is greater than 0, it means that you need to wait until them are committed .

     Param:

     - op=nowait: return after binlog status is recoverd, do not wait until the skipped-binlog transactions are committed.
     - op=reset: reset `SkippedCommitterCounter` to 0 to avoid the problem that `SkippedCommitterCounter` is not cleared due to some unusual cases.
     - op=status: Get the current status of binlog recovery.

32. Enable/disable async commit feature

     ```shell
     curl -X POST -d "tidb_enable_async_commit=1" http://{TiDBIP}:10080/settings
     curl -X POST -d "tidb_enable_async_commit=0" http://{TiDBIP}:10080/settings
     ```

33. Enable/disable one-phase commit feature

     ```shell
     curl -X POST -d "tidb_enable_1pc=1" http://{TiDBIP}:10080/settings
     curl -X POST -d "tidb_enable_1pc=0" http://{TiDBIP}:10080/settings
     ```

34. Enable/disable the mutation checker

     ```shell
     curl -X POST -d "tidb_enable_mutation_checker=1" http://{TiDBIP}:10080/settings
     curl -X POST -d "tidb_enable_mutation_checker=0" http://{TiDBIP}:10080/settings
     ```

35. Get/Set the size of the Ballast Object

     ```shell
     # get current size of the ballast object
     curl -v http://{TiDBIP}:10080/debug/ballast-object-sz
     # reset the size of the ballast object (2GB in this example)
     curl -v -X POST -d "2147483648" http://{TiDBIP}:10080/debug/ballast-object-sz
     ```

36. Set deadlock history table capacity

     ```shell
     curl -X POST -d "deadlock_history_capacity={number}" http://{TiDBIP}:10080/settings
     ```

37. Set whether deadlock history (`DEADLOCKS`) collect retryable deadlocks

     ```shell
     curl -X POST -d "deadlock_history_collect_retryable={bool_val}" http://{TiDBIP}:10080/settings
     ```

38. Set transaction_id to digest mapping minimum duration threshold, only transactions which last longer than this threshold will be collected into `TRX_SUMMARY`.

     ```shell
     curl -X POST -d "transaction_id_digest_min_duration={number}" http://{TiDBIP}:10080/settings
     ```

     Unit of duration here is ms.

39. Set transaction summary table (`TRX_SUMMARY`) capacity

     ```shell
     curl -X POST -d "transaction_summary_capacity={number}" http://{TiDBIP}:10080/settings
     ```

40. The commands are used to handle smooth upgrade mode(refer to the [TiDB Smooth Upgrade](https://github.com/pingcap/docs/blob/4aa0b1d5078617cc06bd1957c5c93e86efb4668d/smooth-upgrade-tidb.md) for details) operations. We can send these upgrade operations to the cluster. The operations here include `start`, `finish` and `show`.

    ```shell
    curl -X POST http://{TiDBIP}:10080/upgrade/{op}
    ```

    ```shell
    $curl -X POST http://{TiDBIP}:10080/upgrade/start
    "success!"
    ```

41. Set split & scatter regions concurrency before ingest, and ingest request concurrency. Value ranges:
    - `max-batch-split-ranges`: `[1, 9223372036854775807]`, default `2048`
    - `max-split-ranges-per-sec`: `[0, 9223372036854775807]`, default `0` (no limit)
    - `max-ingest-per-sec`: `[0, 9223372036854775807]`, default `0` (no limit)
    - `max-ingest-inflight`: `[0, 9223372036854775807]`, default `0` (no limit)

    ```shell
    curl http://{TiDBIP}:10080/ingest/max-batch-split-ranges
    curl http://{TiDBIP}:10080/ingest/max-split-ranges-per-sec
    curl http://{TiDBIP}:10080/ingest/max-ingest-per-sec
    curl http://{TiDBIP}:10080/ingest/max-ingest-inflight
    ```

    ```shell
    curl http://{TiDBIP}:10080/ingest/max-batch-split-ranges -X POST -d "{\"value\": 1024}"
    curl http://{TiDBIP}:10080/ingest/max-split-ranges-per-sec -X POST -d "{\"value\": 16}"
    curl http://{TiDBIP}:10080/ingest/max-ingest-per-sec -X POST -d "{\"value\": 0.5}"
    curl http://{TiDBIP}:10080/ingest/max-ingest-inflight -X POST -d "{\"value\": 2}"
    ```

42. Get TiDB transaction GC states:

     ```shell
     curl http://{TiDBIP}:10080/txn-gc-states
     ```
