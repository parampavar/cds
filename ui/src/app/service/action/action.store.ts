import { Injectable } from '@angular/core';
import { OrderedMap } from 'immutable';
import { BehaviorSubject, Observable } from 'rxjs';
import { Action } from '../../model/action.model';
import { ActionService } from './action.service';

@Injectable()
export class ActionStore {
    actions: BehaviorSubject<OrderedMap<string, Action>> = new BehaviorSubject(OrderedMap<string, Action>());
    projectKey: string;
    groupID: number;

    constructor(private _actionService: ActionService) { }

    getProjectActions(projectKey: string): Observable<OrderedMap<string, Action>> {
        if (this.actions.getValue().size === 0 || this.projectKey !== projectKey) {
            this.projectKey = projectKey;
            this.resyncForProject();
        }
        return new Observable<OrderedMap<string, Action>>(fn => this.actions.subscribe(fn));
    }

    getGroupActions(groupID: number): Observable<OrderedMap<string, Action>> {
        if (this.actions.getValue().size === 0) {
            this.groupID = groupID;
            this.resyncForGroup();
        }
        return new Observable<OrderedMap<string, Action>>(fn => this.actions.subscribe(fn));
    }

    resyncForProject(): void {
        this._actionService.getAllForProject(this.projectKey).subscribe(res => {
            let map = OrderedMap<string, Action>();
            if (res && res.length > 0) {
                res.forEach(a => {
                    map = map.set(a.name, a);
                });
            }
            this.actions.next(map);
        });
    }

    resyncForGroup(): void {
        this._actionService.getAllForGroup(this.groupID).subscribe(res => {
            let map = OrderedMap<string, Action>();
            if (res && res.length > 0) {
                res.forEach(a => {
                    map = map.set(a.name, a);
                });
            }
            this.actions.next(map);
        });
    }
}
