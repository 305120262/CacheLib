# 命令行使用说明
## cachemerge.exe

|参数|含义|备注
| :--- | :--- | :--- |
|origin|原缓存瓦片路径|包含_alllayers和conf.xml的缓存瓦片目录或者是tpkx的路径
|overlayer|叠加缓存瓦片路径|包含_alllayers和conf.xml的缓存瓦片目录或者是tpkx的路径
|overwrite|可选是否直接更新原缓存瓦片(true|false)|默认为false，当设置为true时，不要设置output，并请注意备份好原缓存瓦片！
|output|合成后的缓存瓦片路径|缓存瓦片目录或者是tpkx格式路径，和origin保存一致
|workers|并发处理数目|默认为20，必须为整数，建议按实际可用CPU核设置

